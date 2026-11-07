//go:build integration

package integration

import (
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func protoJSON(m proto.Message) ([]byte, error) { return protojson.Marshal(m) }
