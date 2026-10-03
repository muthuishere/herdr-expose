package chat

import (
	"encoding/json"
	"errors"
	"time"
)

// callTimeout bounds one Herdr method call made on a chat message's behalf.
// Short, because somebody is waiting in a chat window: a prompt that has not
// been accepted in this long is better reported than waited on.
const callTimeout = 15 * time.Second

func decode(line []byte, v any) error { return json.Unmarshal(line, v) }

// asGiveUpErr is errors.As for a *GiveUp, kept here so manager.go reads as
// intent rather than as plumbing.
func asGiveUpErr(err error, out **GiveUp) bool { return errorsAs(err, out) }

func errorsAs(err error, target any) bool { return errors.As(err, target) }
