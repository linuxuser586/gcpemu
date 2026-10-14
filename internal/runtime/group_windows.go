package runtime

// fileGroup is unknown on Windows, which has no Unix group ids.
func fileGroup(string) (int, bool) { return 0, false }
