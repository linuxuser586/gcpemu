// Package apidef holds the API surface generated from Google's published
// definitions (NFR-MNT-001): the google.api.http routes and field
// behaviours (AIP-203) of the gRPC APIs, read from googleapis at the
// commit pinned in apis.yaml, and the methods and field annotations of the
// discovery (JSON/REST) APIs, read from the discovery documents vendored in
// the google.golang.org/api module pinned in go.mod. A version bump of
// either is followed by `make generate`, which rewrites zz_*.go.
//
// Services use it to route REST onto their gRPC implementations
// (internal/transcode) and to validate requests: CheckProto for protobuf
// requests, CheckJSON for discovery request bodies.
package apidef

//go:generate go run ./internal/apigen -config apis.yaml -out .

// Behavior is a set of google.api.FieldBehavior values.
type Behavior uint8

const (
	// Required fields must be set by the caller.
	Required Behavior = 1 << iota
	// OutputOnly fields are set by the server; input values are ignored.
	OutputOnly
	// Immutable fields may be set on create but never changed.
	Immutable
	// InputOnly fields are accepted on input and never returned.
	InputOnly
	// Identifier marks a resource's name field.
	Identifier
	// Optional fields are explicitly optional.
	Optional
)

// Service is a gRPC service and the HTTP bindings of its methods.
type Service struct {
	Name    string // fully qualified, e.g. google.pubsub.v1.Publisher
	File    string // googleapis path of the defining .proto
	Methods []*Method
}

// Method is one RPC.
type Method struct {
	Name   string
	Input  string // request message full name
	Output string // response message full name
	HTTP   []Binding
}

// Binding is one google.api.http rule (the primary rule or an additional
// binding).
type Binding struct {
	Method string // GET, POST, PATCH, PUT, DELETE or a custom verb
	Path   string // path template, e.g. /v1/{parent=projects/*}/topics
	Body   string // "", "*" or a request field name
}

// FullMethod returns the gRPC full method name of m in s.
func (s *Service) FullMethod(m *Method) string { return "/" + s.Name + "/" + m.Name }

// Method returns the named method, or nil.
func (s *Service) Method(name string) *Method {
	for _, m := range s.Methods {
		if m.Name == name {
			return m
		}
	}
	return nil
}

var (
	services       = map[string]*Service{}
	methodsByRPC   = map[string]*Method{}
	methodsByInput = map[string]string{} // request message → full method; "" if shared
)

func init() {
	for _, s := range protoServices {
		services[s.Name] = s
		for _, m := range s.Methods {
			methodsByRPC[s.FullMethod(m)] = m
			if _, dup := methodsByInput[m.Input]; dup {
				methodsByInput[m.Input] = ""
			} else {
				methodsByInput[m.Input] = s.FullMethod(m)
			}
		}
	}
	for _, a := range discoveryAPIs {
		restAPIs[a.Name+"/"+a.Version] = a
		a.index()
	}
}

// ServiceByName returns the generated definition of a gRPC service, or nil.
func ServiceByName(name string) *Service { return services[name] }

// MethodByRPC returns the method for a gRPC full method name
// ("/google.pubsub.v1.Publisher/CreateTopic"), or nil.
func MethodByRPC(fullMethod string) *Method { return methodsByRPC[fullMethod] }

// RPCForInput returns the full method name of the one RPC taking request
// messages of the given type, or "" if none or several do (the IAM and
// operations requests are shared by every API).
func RPCForInput(message string) string { return methodsByInput[message] }

// FieldBehaviors returns the annotated fields of a message (by proto
// field name), or nil if none of its fields carry a behaviour.
func FieldBehaviors(message string) map[string]Behavior { return protoFields[message] }

// GoogleapisCommit is the googleapis/googleapis commit the proto tables
// were generated from.
func GoogleapisCommit() string { return googleapisCommit }
