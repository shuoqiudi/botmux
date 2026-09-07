package gateway

import "bytes"

func jsonBytesReader(value []byte) *bytes.Reader { return bytes.NewReader(value) }
