package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Happy2018new/local-ai-relay/internal/config"
	"github.com/Happy2018new/local-ai-relay/internal/credentials"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"golang.org/x/term"
)

type Result struct {
	Config config.Config
	Saved  bool
	Start  bool
}

type editor struct {
	path     string
	cfg      config.Config
	original config.Config
	exists   bool
	notice   string
	run      func(*huh.Form) error
	legacy   credentials.Reader
}

func Available() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

func Run(path, logOverride string) (Result, error) {
	if !Available() {
		return Result{}, errors.New("TUI requires an interactive terminal; run local-ai-relay -tui in Windows Terminal or another terminal")
	}
	e, err := openEditor(path)
	if err != nil {
		return Result{}, err
	}
	if logOverride != "" {
		absolute, err := filepath.Abs(logOverride)
		if err != nil {
			return Result{}, err
		}
		e.cfg.LogFile = absolute
	}
	e.run = runForm
	result, err := e.loop()
	if errors.Is(err, huh.ErrUserAborted) {
		return Result{}, nil
	}
	return result, err
}

func openEditor(path string) (*editor, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadFile(absolute)
	exists := err == nil
	if errors.Is(err, os.ErrNotExist) {
		cfg = config.Default()
	} else if err != nil {
		return nil, fmt.Errorf("read configuration for TUI: %w", err)
	}
	e := &editor{path: absolute, cfg: cfg, original: cfg, exists: exists, legacy: credentials.System{}}
	if e.cfg.LogFile == "" {
		e.cfg.LogFile = filepath.Join(".runtime", "relay.log")
	}
	if !exists {
		e.original = e.cfg
	}
	return e, nil
}

func (e *editor) loop() (Result, error) {
	for {
		action := "connection"
		if e.cfg.UpstreamURL != "" {
			action = "start"
		}
		if err := e.run(e.menu(&action)); err != nil {
			return Result{}, err
		}
		e.notice = ""
		switch action {
		case "start", "save":
			result, err := e.save(action == "start")
			if err != nil {
				e.notice = err.Error()
				continue
			}
			return result, nil
		case "quit":
			if e.dirty() {
				discard := false
				form := e.form(huh.NewGroup(huh.NewConfirm().Key("discard").Title("放弃未保存的修改？").
					Affirmative("放弃修改").Negative("继续编辑").Value(&discard)))
				if err := e.run(form); err != nil {
					return Result{}, err
				}
				if !discard {
					continue
				}
			}
			return Result{}, nil
		case "logs":
			if err := ViewLogs(context.Background(), e.cfg.LogPath(e.path), "运行日志"); err != nil {
				e.notice = err.Error()
			}
		default:
			if err := e.edit(action); err != nil {
				if errors.Is(err, huh.ErrUserAborted) {
					continue
				}
				return Result{}, err
			}
		}
	}
}

func (e *editor) menu(action *string) *huh.Form {
	state := "新配置"
	if e.exists {
		state = "已保存"
	}
	if e.dirty() {
		state = "未保存"
	}
	upstream := e.cfg.UpstreamURL
	if upstream == "" {
		upstream = "未设置"
	}
	stream := "开启"
	if !e.cfg.RequireStream {
		stream = "关闭"
	}
	description := fmt.Sprintf("%s · %s\n上游 %s\n本地 %s · 并发 %d · 流模式 %s", state, filepath.Base(e.path), upstream, e.cfg.Listen, e.cfg.MaxConcurrent, stream)
	if e.notice != "" {
		description += "\n\n错误  " + e.notice
	}
	return e.form(huh.NewGroup(huh.NewSelect[string]().Key("action").Title("local-ai-relay").Description(description).
		Options(
			huh.NewOption("上游与 API Key", "connection"),
			huh.NewOption("本地设置", "local"),
			huh.NewOption("队列与并发", "queue"),
			huh.NewOption("重试策略", "retry"),
			huh.NewOption("超时与请求限制", "limits"),
			huh.NewOption("流模式", "auth"),
			huh.NewOption("查看日志", "logs"),
			huh.NewOption("保存并启动", "start"),
			huh.NewOption("保存并退出", "save"),
			huh.NewOption("退出而不保存", "quit"),
		).Value(action)))
}

func (e *editor) dirty() bool {
	return e.cfg != e.original
}

func (e *editor) form(groups ...*huh.Group) *huh.Form {
	form := huh.NewForm(groups...).WithTheme(huh.ThemeBase()).WithShowHelp(false).WithAccessible(false)
	if width, height, err := term.GetSize(int(os.Stderr.Fd())); err == nil {
		form.WithWidth(max(20, min(width-2, 88))).WithHeight(max(height-2, 8))
	}
	return form
}

type responsiveForm struct{ form *huh.Form }

func (m responsiveForm) Init() tea.Cmd { return m.form.Init() }
func (m responsiveForm) View() string  { return m.form.View() }
func (m responsiveForm) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if size, ok := message.(tea.WindowSizeMsg); ok {
		m.form.WithWidth(max(1, min(size.Width-2, 88))).WithHeight(max(size.Height-2, 1))
	}
	_, cmd := m.form.Update(message)
	return m, cmd
}
func runForm(form *huh.Form) error {
	form.SubmitCmd, form.CancelCmd = tea.Quit, tea.Interrupt
	_, err := tea.NewProgram(responsiveForm{form}, tea.WithOutput(os.Stderr)).Run()
	if errors.Is(err, tea.ErrInterrupted) {
		return huh.ErrUserAborted
	}
	return err
}

func (e *editor) save(start bool) (Result, error) {
	if err := e.cfg.Validate(); err != nil {
		return Result{}, err
	}
	if e.exists {
		current, err := config.LoadFile(e.path)
		if err != nil {
			return Result{}, err
		}
		if current != e.original {
			return Result{}, errors.New("配置文件已在外部更改，请重新打开配置界面")
		}
	} else if _, err := os.Stat(e.path); err == nil {
		return Result{}, errors.New("配置文件已在外部创建，请重新打开配置界面")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	draft, err := e.cfg.InlineLegacyKeys(e.legacy)
	if err != nil {
		return Result{}, err
	}
	if start {
		if _, err := draft.Secrets(); err != nil {
			return Result{}, err
		}
	}
	if err := config.Save(e.path, draft); err != nil {
		return Result{}, err
	}
	e.cfg, e.original, e.exists = draft, draft, true
	return Result{Config: draft, Saved: true, Start: start}, nil
}

func (e *editor) edit(section string) error {
	draft := e.cfg
	fields := &fields{}
	var group *huh.Group
	switch section {
	case "connection":
		profile := "current"
		if err := e.run(e.form(huh.NewGroup(huh.NewSelect[string]().Key("profile").Title("上游配置").Options(
			huh.NewOption("当前地址 / 自定义", "current"),
			huh.NewOption("Nexavlinks", "nexavlinks"),
			huh.NewOption("OpenAI", "openai"),
		).Value(&profile)))); err != nil {
			return err
		}
		switch profile {
		case "nexavlinks":
			draft.UpstreamURL = "https://www.nexavlinks.com"
		case "openai":
			draft.UpstreamURL = "https://api.openai.com/v1"
		}
		group = huh.NewGroup(
			huh.NewInput().Key("upstream_url").Title("上游 API 根地址").Placeholder("https://example.com/v1").Value(&draft.UpstreamURL).Validate(func(value string) error {
				candidate := config.Default()
				candidate.UpstreamURL = value
				return candidate.Validate()
			}),
		)
	case "local":
		group = huh.NewGroup(
			huh.NewInput().Key("listen").Title("本地监听地址").Value(&draft.Listen).Validate(func(value string) error {
				candidate := config.Default()
				candidate.UpstreamURL = "https://example.com"
				candidate.Listen = value
				return candidate.Validate()
			}),
			huh.NewInput().Key("log_file").Title("日志文件").Value(&draft.LogFile).Validate(func(value string) error {
				if strings.TrimSpace(value) == "" {
					return errors.New("请选择日志文件路径")
				}
				return nil
			}),
		)
	case "queue":
		group = huh.NewGroup(
			fields.integer("max_concurrent", "最大并发", &draft.MaxConcurrent, 1, 1024),
			fields.integer("max_queue", "最大排队数", &draft.MaxQueue, 0, 100000),
			fields.duration("min_interval", "请求最小间隔", &draft.MinInterval, true),
			fields.duration("queue_timeout", "排队超时", &draft.QueueTimeout, false),
		)
	case "retry":
		group = huh.NewGroup(
			fields.integer("max_attempts", "总尝试次数（含首次）", &draft.MaxAttempts, 1, 100),
			fields.duration("retry_window", "重试窗口", &draft.RetryWindow, false),
			fields.duration("initial_backoff", "初始退避", &draft.InitialBackoff, false),
			fields.duration("max_backoff", "最大退避", &draft.MaxBackoff, false),
		)
	case "limits":
		group = huh.NewGroup(
			fields.duration("response_header_timeout", "等待上游响应头超时", &draft.ResponseHeaderTimeout, false),
			fields.duration("stream_idle_timeout", "流空闲超时", &draft.StreamIdleTimeout, true),
			fields.duration("error_body_timeout", "读取错误响应超时", &draft.ErrorBodyTimeout, false),
			fields.integer64("max_request_bytes", "请求大小上限（字节）", &draft.MaxRequestBytes, 1, 1<<30),
		)
	case "auth":
		group = huh.NewGroup(
			huh.NewConfirm().Key("require_stream").Title("强制 Responses 流模式").Affirmative("开启").Negative("关闭").Value(&draft.RequireStream),
		)
	default:
		return fmt.Errorf("unknown editor section %q", section)
	}
	if err := e.run(e.form(group)); err != nil {
		return err
	}
	if err := fields.apply(); err != nil {
		return err
	}
	if section == "connection" {
		if err := e.editKey("上游 API Key", "沿用客户端认证", &draft.UpstreamAPIKey, &draft.UpstreamAPIKeyEnv, &draft.UpstreamAPIKeyRef); err != nil {
			return err
		}
	} else if section == "local" {
		if err := e.editKey("本地 API Key", "不启用本地认证", &draft.LocalAPIKey, &draft.LocalAPIKeyEnv, &draft.LocalAPIKeyRef); err != nil {
			return err
		}
	}
	draft.UpstreamURL = strings.TrimSpace(draft.UpstreamURL)
	draft.Listen = strings.TrimSpace(draft.Listen)
	draft.UpstreamAPIKeyEnv = strings.TrimSpace(draft.UpstreamAPIKeyEnv)
	draft.LocalAPIKeyEnv = strings.TrimSpace(draft.LocalAPIKeyEnv)
	e.cfg = draft
	return nil
}

func (e *editor) editKey(title, disabledLabel string, key, env, reference *string) error {
	mode := "disabled"
	if *env != "" {
		mode = "env"
	} else if *key != "" || *reference != "" {
		mode = "config"
	}
	if err := e.run(e.form(huh.NewGroup(huh.NewSelect[string]().Key("key_source").Title(title).Options(
		huh.NewOption(disabledLabel, "disabled"),
		huh.NewOption("输入密钥 · 保存到配置文件", "config"),
		huh.NewOption("环境变量", "env"),
	).Value(&mode)))); err != nil {
		return err
	}
	switch mode {
	case "disabled":
		*key, *env, *reference = "", "", ""
	case "env":
		name := *env
		if err := e.run(e.form(huh.NewGroup(huh.NewInput().Key("key_env").Title("环境变量名").Value(&name).Validate(func(value string) error {
			if strings.TrimSpace(value) == "" {
				return errors.New("请输入环境变量名")
			}
			return validateEnvName(value)
		})))); err != nil {
			return err
		}
		*key, *env, *reference = "", strings.TrimSpace(name), ""
	case "config":
		value := ""
		placeholder := "API Key"
		if *key != "" || *reference != "" {
			placeholder = "留空保留现有密钥"
		}
		if err := e.run(e.form(huh.NewGroup(huh.NewInput().Key("api_key").Title(title).Description("保存到 " + e.path).EchoMode(huh.EchoModePassword).Placeholder(placeholder).Value(&value).Validate(func(value string) error {
			if strings.TrimSpace(value) == "" && *key == "" && *reference == "" {
				return errors.New("请输入 API Key")
			}
			if strings.ContainsAny(value, "\x00\r\n") {
				return errors.New("API Key 不能包含空字符或换行")
			}
			return nil
		})))); err != nil {
			return err
		}
		if value = strings.TrimSpace(value); value != "" {
			*key, *reference = value, ""
		}
		*env = ""
	}
	return nil
}

type fields struct {
	applyValues []func() error
}

func (f *fields) apply() error {
	for _, apply := range f.applyValues {
		if err := apply(); err != nil {
			return err
		}
	}
	return nil
}

func (f *fields) integer(key, title string, destination *int, minimum, maximum int64) *huh.Input {
	value := int64(*destination)
	field := f.integer64(key, title, &value, minimum, maximum)
	f.applyValues = append(f.applyValues, func() error { *destination = int(value); return nil })
	return field
}

func (f *fields) integer64(key, title string, destination *int64, minimum, maximum int64) *huh.Input {
	text := strconv.FormatInt(*destination, 10)
	validate := func(value string) error {
		number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || number < minimum || number > maximum {
			return fmt.Errorf("请输入 %d 到 %d 之间的整数", minimum, maximum)
		}
		return nil
	}
	f.applyValues = append(f.applyValues, func() error {
		if err := validate(text); err != nil {
			return err
		}
		*destination, _ = strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		return nil
	})
	return huh.NewInput().Key(key).Title(title).Value(&text).Validate(validate)
}

func (f *fields) duration(key, title string, destination *config.Duration, zeroAllowed bool) *huh.Input {
	text := destination.Value().String()
	validate := func(value string) error {
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || duration < 0 || duration == 0 && !zeroAllowed {
			return errors.New("请输入有效时长，例如 1s 或 3m")
		}
		return nil
	}
	f.applyValues = append(f.applyValues, func() error {
		if err := validate(text); err != nil {
			return err
		}
		duration, _ := time.ParseDuration(strings.TrimSpace(text))
		*destination = config.Duration(duration)
		return nil
	})
	return huh.NewInput().Key(key).Title(title).Placeholder("1s / 3m").Value(&text).Validate(validate)
}

func validateEnvName(value string) error {
	value = strings.TrimSpace(value)
	for i, char := range value {
		if !(char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char == '_' || i > 0 && char >= '0' && char <= '9') {
			return errors.New("请输入环境变量名，不是密钥值")
		}
	}
	return nil
}
