package credentials

import (
	"crypto/rand"
	"os"
	"testing"
)

func TestSystemCredentialStore(t *testing.T) {
	if os.Getenv("LOCAL_AI_RELAY_TEST_KEYRING") != "1" {
		t.Skip("set LOCAL_AI_RELAY_TEST_KEYRING=1 to test the system credential store")
	}
	store := System{}
	reference := "self-test-" + rand.Text()
	if err := store.Set(reference, "relay-test-value"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(reference) })
	value, err := store.Get(reference)
	if err != nil || value != "relay-test-value" {
		t.Fatal("system credential round trip failed")
	}
}
