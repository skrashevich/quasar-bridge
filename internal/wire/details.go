package wire

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	maxDetailDepth = 64
	hiddenValue    = "[скрыто]"
)

// Details renders populated protobuf fields, including the enclosing message.
// Values are for the interactive diagnostics view only, never for slog.
func (f Frame) Details() any {
	switch {
	case f.Client != nil:
		return messageDetails(f.Client.ProtoReflect(), 0)
	case f.Server != nil:
		return messageDetails(f.Server.ProtoReflect(), 0)
	case f.Legacy != nil:
		return messageDetails(f.Legacy.ProtoReflect(), 0)
	default:
		return nil
	}
}

func messageDetails(m protoreflect.Message, depth int) any {
	if depth >= maxDetailDepth {
		return "[достигнут предел вложенности]"
	}
	switch obj := m.Interface().(type) {
	case *structpb.Struct:
		return cleanJSON(obj.AsMap(), depth)
	case *structpb.ListValue:
		return cleanJSON(obj.AsSlice(), depth)
	case *structpb.Value:
		return cleanJSON(obj.AsInterface(), depth)
	case *anypb.Any:
		out := map[string]any{"@type": obj.MessageName(), "value": binarySize(obj.Value)}
		if nested, err := anypb.UnmarshalNew(obj, proto.UnmarshalOptions{RecursionLimit: maxDetailDepth}); err == nil {
			out["value"] = messageDetails(nested.ProtoReflect(), depth+1)
		}
		return out
	}
	out := make(map[string]any)
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		name := string(f.Name())
		switch {
		case sensitiveField(name):
			out[name] = hiddenValue
		case f.IsMap():
			entries := make(map[string]any)
			v.Map().Range(func(k protoreflect.MapKey, value protoreflect.Value) bool {
				key := k.String()
				if sensitiveField(key) {
					entries[key] = hiddenValue
				} else {
					entries[key] = scalarDetails(f.MapValue(), value, depth)
				}
				return true
			})
			out[name] = entries
		case f.IsList():
			items := make([]any, v.List().Len())
			for i := range items {
				items[i] = scalarDetails(f, v.List().Get(i), depth)
			}
			out[name] = items
		default:
			out[name] = scalarDetails(f, v, depth)
		}
		return true
	})
	maskHeaderValue(out)
	if n := len(m.GetUnknown()); n > 0 {
		out["_unknown_bytes"] = n
	}
	return out
}

func scalarDetails(f protoreflect.FieldDescriptor, v protoreflect.Value, depth int) any {
	switch f.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return messageDetails(v.Message(), depth+1)
	case protoreflect.BytesKind:
		return bytesDetails(f, v.Bytes(), depth)
	case protoreflect.StringKind:
		s := v.String()
		if decoded, ok := embeddedJSON([]byte(s), depth); ok {
			return decoded
		}
		return s
	case protoreflect.EnumKind:
		if e := f.Enum().Values().ByNumber(v.Enum()); e != nil {
			return string(e.Name())
		}
		return int32(v.Enum())
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return strconv.FormatInt(v.Int(), 10)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(v.Uint(), 10)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		if n := v.Float(); math.IsNaN(n) || math.IsInf(n, 0) {
			return fmt.Sprint(n)
		}
	}
	return v.Interface()
}

// Some oneofs transmit the serialized form of their sibling message, e.g.
// DeviceStateRaw and DeviceState. Other bytes (such as audio) stay opaque.
func bytesDetails(f protoreflect.FieldDescriptor, raw []byte, depth int) any {
	name, ok := strings.CutSuffix(string(f.Name()), "Raw")
	if !ok || f.ContainingOneof() == nil {
		return binarySize(raw)
	}
	if decoded, ok := embeddedJSON(raw, depth); ok {
		return decoded
	}
	peer := f.ContainingOneof().Fields().ByName(protoreflect.Name(name))
	if peer == nil || peer.Message() == nil || depth >= maxDetailDepth {
		return binarySize(raw)
	}
	mt, err := protoregistry.GlobalTypes.FindMessageByName(peer.Message().FullName())
	if err != nil {
		return binarySize(raw)
	}
	msg := mt.New()
	if err := (proto.UnmarshalOptions{RecursionLimit: maxDetailDepth}).Unmarshal(raw, msg.Interface()); err != nil || len(msg.GetUnknown()) != 0 {
		return binarySize(raw)
	}
	return messageDetails(msg, depth+1)
}

func binarySize(b []byte) string {
	return fmt.Sprintf("[двоичные данные, %d байт]", len(b))
}

func embeddedJSON(raw []byte, depth int) (any, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || (raw[0] != '{' && raw[0] != '[') {
		return nil, false
	}
	if !json.Valid(raw) {
		// Keep ordinary bracketed text. A malformed JSON object may still contain credentials.
		if raw[0] == '[' && !bytes.ContainsAny(raw, "\"{") {
			return nil, false
		}
		return "[некорректный JSON]", true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return "[некорректный JSON]", true
	}
	return cleanJSON(value, depth+1), true
}

func cleanJSON(value any, depth int) any {
	if depth >= maxDetailDepth {
		return "[достигнут предел вложенности]"
	}
	switch obj := value.(type) {
	case map[string]any:
		for k, v := range obj {
			if sensitiveField(k) {
				obj[k] = hiddenValue
			} else {
				obj[k] = cleanJSON(v, depth+1)
			}
		}
		maskHeaderValue(obj)
	case []any:
		for i, v := range obj {
			obj[i] = cleanJSON(v, depth+1)
		}
	case json.Number:
		// Preserve large integer identifiers in JavaScript clients.
		s := obj.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := obj.Int64(); err != nil || n > 1<<53-1 || n < -(1<<53-1) {
				return s
			}
		}
		return jsontext.Value(s)
	case string:
		if decoded, ok := embeddedJSON([]byte(obj), depth); ok {
			return decoded
		}
	}
	return value
}

// Dynamic HTTP headers may use {name, value} instead of field names.
func maskHeaderValue(obj map[string]any) {
	for k, v := range obj {
		if strings.EqualFold(k, "name") || strings.EqualFold(k, "key") {
			if name, ok := v.(string); ok && sensitiveField(name) {
				for key := range obj {
					if strings.EqualFold(key, "value") {
						obj[key] = hiddenValue
					}
				}
			}
		}
	}
}

var fieldSeparators = strings.NewReplacer("_", "", "-", "")

func sensitiveField(name string) bool {
	key := fieldSeparators.Replace(strings.ToLower(name))
	for _, word := range []string{"token", "cookie", "password", "secret", "authorization", "credential", "privatekey", "apikey", "ticket", "signature"} {
		if strings.Contains(key, word) {
			return true
		}
	}
	return key == "oauth" || key == "auth" || strings.HasSuffix(key, "sessionkey") || strings.HasSuffix(key, "url") || strings.HasSuffix(key, "uri")
}
