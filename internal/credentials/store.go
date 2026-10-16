package credentials

import "github.com/zalando/go-keyring"

const service = "local-ai-relay"

// Reader supports importing configurations created before keys were stored in JSON.
type Reader interface {
	Get(reference string) (string, error)
}

type System struct{}

func (System) Get(reference string) (string, error) { return keyring.Get(service, reference) }
