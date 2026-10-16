package credentials

import "github.com/zalando/go-keyring"

const service = "local-ai-relay"

type Store interface {
	Get(reference string) (string, error)
	Set(reference, value string) error
	Delete(reference string) error
}

type System struct{}

func (System) Get(reference string) (string, error) { return keyring.Get(service, reference) }
func (System) Set(reference, value string) error    { return keyring.Set(service, reference, value) }
func (System) Delete(reference string) error        { return keyring.Delete(service, reference) }
