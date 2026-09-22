package delivery

import "errors"

var ErrNotAccepted = errors.New("the provider did not accept the message")
