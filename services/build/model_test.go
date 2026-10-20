package build

import "testing"

// A Build stored before records kept its provided built-ins decodes
// without them, so its retry provides none, as before.
func TestDecodeWithoutProvided(t *testing.T) {
	st, err := decode([]byte(`{"build":{"name":"projects/p/locations/global/builds/b","id":"b"},"request":{"steps":[{"name":"busybox"}]},"op":"operations/o"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.build.GetId() != "b" || len(st.request.GetSteps()) != 1 || st.op != "operations/o" || st.provided != nil {
		t.Errorf("decoded = %v %v %q provided %v", st.build, st.request, st.op, st.provided)
	}
}
