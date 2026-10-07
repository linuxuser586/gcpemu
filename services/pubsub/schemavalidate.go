package pubsub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/bufbuild/protocompile"
	"github.com/hamba/avro/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Schema validation (FR-PS-009): Avro via hamba/avro (MIT) and Protocol
// Buffers via protocompile (Apache-2.0) + dynamicpb.

// validator checks one encoded message against one schema revision.
type validator interface {
	validate(data []byte, enc pubsubpb.Encoding) error
}

// compileSchema parses a schema definition.
func compileSchema(typ pubsubpb.Schema_Type, def string) (validator, error) {
	if def == "" {
		return nil, errors.New("schema definition must not be empty")
	}
	switch typ {
	case pubsubpb.Schema_AVRO:
		sc, err := avro.ParseWithCache(def, "", &avro.SchemaCache{})
		if err != nil {
			return nil, fmt.Errorf("invalid Avro schema: %w", err)
		}
		return avroValidator{sc}, nil
	case pubsubpb.Schema_PROTOCOL_BUFFER:
		md, err := compileProto(def)
		if err != nil {
			return nil, err
		}
		return protoValidator{md}, nil
	}
	return nil, errors.New("schema type must be AVRO or PROTOCOL_BUFFER")
}

// compileProto compiles a .proto definition and returns its first message.
func compileProto(def string) (protoreflect.MessageDescriptor, error) {
	const file = "schema.proto"
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: protocompile.SourceAccessorFromMap(map[string]string{file: def}),
		}),
	}
	files, err := c.Compile(context.Background(), file)
	if err != nil {
		return nil, fmt.Errorf("invalid Protocol Buffer schema: %w", err)
	}
	msgs := files[0].Messages()
	if msgs.Len() == 0 {
		return nil, errors.New("invalid Protocol Buffer schema: the definition must contain at least one message")
	}
	return msgs.Get(0), nil
}

type protoValidator struct {
	md protoreflect.MessageDescriptor
}

func (v protoValidator) validate(data []byte, enc pubsubpb.Encoding) error {
	m := dynamicpb.NewMessage(v.md)
	if enc == pubsubpb.Encoding_BINARY {
		return proto.Unmarshal(data, m)
	}
	return protojson.Unmarshal(data, m)
}

type avroValidator struct{ sc avro.Schema }

func (v avroValidator) validate(data []byte, enc pubsubpb.Encoding) error {
	if enc == pubsubpb.Encoding_BINARY {
		var out any
		return avro.Unmarshal(v.sc, data, &out)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return checkAvroJSON(v.sc, val, "$")
}

// checkAvroJSON validates a value in Avro's JSON encoding. Union values may
// be given either wrapped ({"type": value}) or bare.
func checkAvroJSON(sc avro.Schema, v any, path string) error {
	if ref, ok := sc.(*avro.RefSchema); ok {
		sc = ref.Schema()
	}
	bad := func() error { return fmt.Errorf("%s: value does not match Avro type %s", path, sc.Type()) }
	switch sc.Type() {
	case avro.Null:
		if v != nil {
			return bad()
		}
	case avro.Boolean:
		if _, ok := v.(bool); !ok {
			return bad()
		}
	case avro.Int, avro.Long:
		n, ok := v.(json.Number)
		if !ok {
			return bad()
		}
		i, err := n.Int64()
		if err != nil || (sc.Type() == avro.Int && (i < math.MinInt32 || i > math.MaxInt32)) {
			return bad()
		}
	case avro.Float, avro.Double:
		n, ok := v.(json.Number)
		if !ok {
			if s, ok := v.(string); ok && (s == "NaN" || s == "Infinity" || s == "-Infinity") {
				return nil
			}
			return bad()
		}
		if _, err := n.Float64(); err != nil {
			return bad()
		}
	case avro.String, avro.Bytes:
		if _, ok := v.(string); !ok {
			return bad()
		}
	case avro.Fixed:
		s, ok := v.(string)
		if !ok || len([]rune(s)) != sc.(*avro.FixedSchema).Size() {
			return bad()
		}
	case avro.Enum:
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		for _, sym := range sc.(*avro.EnumSchema).Symbols() {
			if sym == s {
				return nil
			}
		}
		return fmt.Errorf("%s: %q is not a symbol of enum %s", path, s, sc.(*avro.EnumSchema).FullName())
	case avro.Array:
		arr, ok := v.([]any)
		if !ok {
			return bad()
		}
		items := sc.(*avro.ArraySchema).Items()
		for i, x := range arr {
			if err := checkAvroJSON(items, x, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case avro.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return bad()
		}
		vals := sc.(*avro.MapSchema).Values()
		for k, x := range obj {
			if err := checkAvroJSON(vals, x, path+"."+k); err != nil {
				return err
			}
		}
	case avro.Record, avro.Error:
		obj, ok := v.(map[string]any)
		if !ok {
			return bad()
		}
		rs := sc.(*avro.RecordSchema)
		known := map[string]bool{}
		for _, f := range rs.Fields() {
			known[f.Name()] = true
			x, present := obj[f.Name()]
			if !present {
				if f.HasDefault() || isNullable(f.Type()) {
					continue
				}
				return fmt.Errorf("%s: missing required field %q", path, f.Name())
			}
			if err := checkAvroJSON(f.Type(), x, path+"."+f.Name()); err != nil {
				return err
			}
		}
		for k := range obj {
			if !known[k] {
				return fmt.Errorf("%s: unknown field %q", path, k)
			}
		}
	case avro.Union:
		types := sc.(*avro.UnionSchema).Types()
		if v == nil {
			for _, t := range types {
				if t.Type() == avro.Null {
					return nil
				}
			}
			return bad()
		}
		if obj, ok := v.(map[string]any); ok && len(obj) == 1 {
			for k, x := range obj {
				for _, t := range types {
					if unionBranchName(t) == k {
						return checkAvroJSON(t, x, path+"."+k)
					}
				}
			}
		}
		for _, t := range types {
			if checkAvroJSON(t, v, path) == nil {
				return nil
			}
		}
		return bad()
	}
	return nil
}

func isNullable(sc avro.Schema) bool {
	if u, ok := sc.(*avro.UnionSchema); ok {
		for _, t := range u.Types() {
			if t.Type() == avro.Null {
				return true
			}
		}
	}
	return sc.Type() == avro.Null
}

// unionBranchName is the key used to wrap a union value in Avro JSON.
func unionBranchName(sc avro.Schema) string {
	if ref, ok := sc.(*avro.RefSchema); ok {
		sc = ref.Schema()
	}
	if n, ok := sc.(avro.NamedSchema); ok {
		return n.FullName()
	}
	return string(sc.Type())
}
