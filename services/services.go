// Package services lists the service modules compiled into gcpemu.
package services

import (
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/services/ar"
	"github.com/linuxuser586/gcpemu/services/compute"
	"github.com/linuxuser586/gcpemu/services/dns"
	"github.com/linuxuser586/gcpemu/services/gcs"
	"github.com/linuxuser586/gcpemu/services/gke"
	"github.com/linuxuser586/gcpemu/services/iam"
	"github.com/linuxuser586/gcpemu/services/nat"
	"github.com/linuxuser586/gcpemu/services/pubsub"
	"github.com/linuxuser586/gcpemu/services/sql"
)

// Factories returns a constructor for every implemented service.
func Factories() map[string]instance.Factory {
	return map[string]instance.Factory{
		"iam":     iam.New,
		"compute": compute.New,
		"dns":     dns.New,
		"ar":      ar.New,
		"gcs":     gcs.New,
		"pubsub":  pubsub.New,
		"sql":     sql.New,
		"gke":     gke.New,
		"nat":     nat.New,
	}
}
