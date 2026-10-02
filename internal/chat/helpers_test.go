package chat

import (
	"errors"
	"fmt"
)

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func asGiveUp(err error, out **GiveUp) bool { return errors.As(err, out) }
