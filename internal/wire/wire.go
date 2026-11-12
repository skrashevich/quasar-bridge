package wire

import (
	speechkit "github.com/skrashevich/quasar-go-genproto/proto/alice/cuttlefish/library/speechkit_proto"
	client "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/client"
	server "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/api/alicekit/protocol/server"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type Direction int

const (
	ClientToServer Direction = iota
	ServerToClient
)

type Frame struct {
	Raw    []byte
	Prefix []byte
	Client *client.TClientMessage
	Server *server.TServerMessage
	Legacy *speechkit.TSpeechkitMessage
}

func Decode(data []byte, d Direction) Frame {
	if len(data) > 4 && string(data[:4]) == "AAPI" {
		f := decodeBare(data[4:], d)
		if f.Kind() != "opaque" {
			f.Raw = data
			f.Prefix = data[:4]
			return f
		}
	}
	return decodeBare(data, d)
}

func decodeBare(data []byte, d Direction) Frame {
	f := Frame{Raw: data}
	if d == ClientToServer {
		var m client.TClientMessage
		if proto.Unmarshal(data, &m) == nil {
			if e := m.GetEvent(); e != nil && e.GetHeader() != nil && eventName(e) != "" {
				f.Client = &m
				return f
			}
			if sc := m.GetStreamControl(); sc != nil && sc.GetMessageId() != "" {
				f.Client = &m
				return f
			}
		}
	} else {
		var m server.TServerMessage
		if proto.Unmarshal(data, &m) == nil {
			if e := m.GetEvent(); e != nil && e.GetHeader() != nil && eventName(e) != "" {
				f.Server = &m
				return f
			}
			if sc := m.GetStreamControl(); sc != nil && (sc.GetMessageId() != "" || sc.GetClose() != nil && sc.GetStreamId() != 0) {
				f.Server = &m
				return f
			}
		}
	}
	var legacy speechkit.TSpeechkitMessage
	if proto.Unmarshal(data, &legacy) == nil && legacy.GetHeader().GetName() != "" && legacy.GetPayload().GetTypeUrl() != "" {
		f.Legacy = &legacy
	}
	return f
}
func eventName(m proto.Message) string {
	r := m.ProtoReflect()
	one := r.Descriptor().Oneofs().ByName(protoreflect.Name("Event"))
	if one == nil {
		return ""
	}
	if field := r.WhichOneof(one); field != nil {
		return string(field.Name())
	}
	return ""
}
func (f Frame) Kind() string {
	if f.Client != nil {
		if e := f.Client.GetEvent(); e != nil {
			return eventName(e)
		}
		return "StreamControl"
	}
	if f.Server != nil {
		if e := f.Server.GetEvent(); e != nil {
			return eventName(e)
		}
		return "StreamControl"
	}
	if f.Legacy != nil {
		return f.Legacy.GetHeader().GetName()
	}
	return "opaque"
}
func (f Frame) Marshal() ([]byte, error) {
	data, err := f.marshalBare()
	if err != nil {
		return nil, err
	}
	if len(f.Prefix) > 0 {
		return append(append([]byte{}, f.Prefix...), data...), nil
	}
	return data, nil
}
func (f Frame) marshalBare() ([]byte, error) {
	if f.Client != nil {
		return proto.Marshal(f.Client)
	}
	if f.Server != nil {
		return proto.Marshal(f.Server)
	}
	if f.Legacy != nil {
		return proto.Marshal(f.Legacy)
	}
	return f.Raw, nil
}
