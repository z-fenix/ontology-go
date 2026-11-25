package workflow

import (
	"errors"

	"go.temporal.io/sdk/temporal"
)

func asApp(err error, target **temporal.ApplicationError) bool { return errors.As(err, target) }
