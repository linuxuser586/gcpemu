// Package compat is the client compatibility suite (SRS 7.1, IF-003 to
// IF-006 and IF-009): the real gcloud, kubectl with gke-gcloud-auth-plugin,
// Helm, Docker, crane, ko, psql, the Cloud SQL Auth Proxy v2 and dig run
// against a detached gcpemu instance with only endpoint configuration.
//
//	go test -tags compat ./compat -v -timeout 30m
//
// gcloud (with the gke-gcloud-auth-plugin component), kubectl, dig and
// Docker come from PATH; their tests skip when one is missing. Helm, crane,
// ko and the Auth Proxy are downloaded at pinned, checksum-verified
// versions into $GCPEMU_COMPAT_CACHE (default <user cache dir>/gcpemu-compat),
// and psql runs from the PostgreSQL image. Cloud SQL and GKE need a
// container runtime; some steps (crane copy from Docker Hub, ko's base
// image) need the internet.
package compat
