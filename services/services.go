// Package services lists the service modules compiled into gcpemu.
package services

import (
	"github.com/linuxuser586/gcpemu/internal/instance"
	"github.com/linuxuser586/gcpemu/services/ar"
	"github.com/linuxuser586/gcpemu/services/dns"
	"github.com/linuxuser586/gcpemu/services/gcs"
	"github.com/linuxuser586/gcpemu/services/iam"
	"github.com/linuxuser586/gcpemu/services/pubsub"
)

// Factories returns a constructor for every implemented service.
func Factories() map[string]instance.Factory {
	return map[string]instance.Factory{
		"iam":    iam.New,
		"dns":    dns.New,
		"ar":     ar.New,
		"gcs":    gcs.New,
		"pubsub": pubsub.New,
	}
}
