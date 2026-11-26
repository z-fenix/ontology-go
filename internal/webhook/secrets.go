package webhook

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// SecretProvider resolves secret references such as env://NAME or vault://path.
type SecretProvider interface {
	Secret(ctx context.Context, ref string) (string, error)
}

// EnvSecrets resolves env://NAME from the process environment (see Secrets for vault://).
type EnvSecrets struct{}

// Secret implements SecretProvider.
func (EnvSecrets) Secret(_ context.Context, ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "env://")
	if !ok {
		if strings.HasPrefix(ref, "vault://") {
			return "", fmt.Errorf("vault secrets need a Vault provider (%s)", ref)
		}
		return "", fmt.Errorf("unsupported secret reference %q", ref)
	}
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("secret %s is not set", ref)
	}
	return v, nil
}
