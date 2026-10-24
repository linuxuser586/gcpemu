package gateway

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/linuxuser586/gcpemu/internal/project"
)

// projectSegment matches the Project in a resource name or REST path:
// projects/P, where P is a Project ID or number.
var projectSegment = regexp.MustCompile(`(?:^|/)projects/([^/:?]+)`)

// bucketCollection matches Cloud Storage's bucket collection, whose
// project parameter names the Project of the buckets it lists or creates.
var bucketCollection = regexp.MustCompile(`^(/storage)?/storage/v1/b/?$`)

// requestProjects returns the Projects a request names, as IDs or numbers,
// in order and each once: by which the API gateway forwards it to the
// Linked Instance they are delegated to, and by which a Linked Instance
// checks that they are shared with the link it came over (ADR 0027).
//
// A REST request names the Projects of the projects/P in its path, or the
// bucket collection's project parameter. A gRPC call names those its
// request message's string fields, and those of the messages nested in
// it, start with projects/P, read by the message's registered descriptor,
// so that fields it does not have, bytes and maps name none. The Admin
// API, the Web console and in-container agents' callbacks name none, nor
// do projects/-, a bucket, or a gRPC method this binary does not know.
func requestProjects(r *http.Request) []string {
	if isGRPC(r) {
		return messageProjects(r)
	}
	if internalPath(r.URL.Path) {
		return nil
	}
	var out []string
	for _, m := range projectSegment.FindAllStringSubmatch(r.URL.EscapedPath(), -1) {
		out = addProject(out, m[1])
	}
	if len(out) == 0 && bucketCollection.MatchString(r.URL.Path) {
		out = addProject(out, r.URL.Query().Get("project"))
	}
	return out
}

// internalPath reports whether path, as unescaped, is the Admin API's, the
// Web console's, or an in-container agent's callback rather than a GCP
// API's.
func internalPath(path string) bool {
	return !isAPIRequest(path) || path == "/console" || strings.HasPrefix(path, "/console/")
}

// addProject adds the Project id to ids unless it is there already or is
// no Project ID or number.
func addProject(ids []string, id string) []string {
	if (project.ValidID(id) || isDigits(id)) && !slices.Contains(ids, id) {
		return append(ids, id)
	}
	return ids
}

// maxNesting bounds how deep messageProjects looks into nested messages.
const maxNesting = 8

// messageProjects returns the Projects that the request message of the
// gRPC call r names.
func messageProjects(r *http.Request) []string {
	service, method, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok {
		return nil
	}
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return nil
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		return nil
	}
	b := firstMessage(r)
	if b == nil {
		return nil
	}
	m := dynamicpb.NewMessage(md.Input())
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, m); err != nil {
		return nil
	}
	var out []string
	walkProjects(m, 0, &out)
	return out
}

// walkProjects adds to out the Projects that m's string fields, and those
// of the messages nested in it, name.
func walkProjects(m protoreflect.Message, depth int, out *[]string) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
		case fd.IsList():
			l := v.List()
			for i := range l.Len() {
				fieldProjects(fd, l.Get(i), depth, out)
			}
		default:
			fieldProjects(fd, v, depth, out)
		}
		return true
	})
}

// fieldProjects adds to out the Project that v, a value of the field fd,
// names.
func fieldProjects(fd protoreflect.FieldDescriptor, v protoreflect.Value, depth int, out *[]string) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		if m := projectSegment.FindStringSubmatch(v.String()); m != nil && strings.HasPrefix(v.String(), "projects/") {
			*out = addProject(*out, m[1])
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if depth < maxNesting {
			walkProjects(v.Message(), depth+1, out)
		}
	}
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
