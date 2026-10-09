package gke

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// The SecretSync controller (secretSyncConfig, secret-sync.gke.io/v1). It
// runs per cluster in the emulator while the setting is enabled. Each
// SecretSync names a SecretProviderClass (provider gke) and a Kubernetes
// service account; the controller reads the listed secrets as that account
// (secretaccess.go) and writes them to the Kubernetes Secret named like the
// SecretSync, owned by it, so deleting the SecretSync deletes the Secret.
// A SecretSync is synced when created or changed, when its Secret is
// missing, after a failure, and, with rotation enabled, every rotation
// interval; the Secret is only rewritten when its content changes.

const (
	secretSyncInterval = 2 * time.Second
	secretSyncRetry    = 10 * time.Second
	secretSyncPath     = "/apis/secret-sync.gke.io/v1"
	spcPath            = "/apis/secrets-store.csi.x-k8s.io/v1"
	hashAnnotation     = "secret-sync.gke.io/content-hash"
)

type syncState struct {
	generation int64
	at         time.Time
	failed     bool
}

type secretSync struct {
	Metadata struct {
		objectMeta
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		ServiceAccountName      string `json:"serviceAccountName"`
		SecretProviderClassName string `json:"secretProviderClassName"`
		SecretObject            struct {
			Type        string            `json:"type"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
			Data        []struct {
				SourcePath string `json:"sourcePath"`
				TargetKey  string `json:"targetKey"`
			} `json:"data"`
		} `json:"secretObject"`
	} `json:"spec"`
}

func (s *Service) secretSyncLoop(ctx context.Context, key string) {
	t := time.NewTicker(secretSyncInterval)
	defer t.Stop()
	for {
		if err := s.syncSecretSyncs(ctx, key); err != nil && ctx.Err() == nil {
			s.env.Log.Debug("gke: secret sync", "cluster", key, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) syncSecretSyncs(ctx context.Context, key string) error {
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	c := rec.cluster()
	if !secretSyncOn(c) {
		return nil
	}
	rotate, every := rotation(c.GetSecretSyncConfig().GetRotationConfig())
	kc, err := s.kube(key)
	if err != nil {
		return err
	}
	var list kubeList[secretSync]
	if err := kc.get(ctx, secretSyncPath+"/secretsyncs", &list); err != nil {
		if isKubeNotFound(err) {
			return nil // CRD not installed yet
		}
		return err
	}
	rt := s.rtFor(key)
	now := time.Now()
	for _, ss := range list.Items {
		m := ss.Metadata
		if m.DeletionTimestamp != nil {
			continue
		}
		st, _ := rt.syncs.Load(m.UID)
		prev, seen := st.(syncState)
		due := !seen || prev.generation != m.Generation ||
			prev.failed && now.Sub(prev.at) >= secretSyncRetry ||
			rotate && now.Sub(prev.at) >= every
		if !due {
			var sec struct {
				Metadata objectMeta `json:"metadata"`
			}
			due = isKubeNotFound(kc.get(ctx, "/api/v1/namespaces/"+m.Namespace+"/secrets/"+m.Name, &sec))
		}
		if !due {
			continue
		}
		err := s.syncOne(ctx, key, kc, &ss)
		rt.syncs.Store(m.UID, syncState{generation: m.Generation, at: now, failed: err != nil})
		cond := map[string]any{"type": "SecretSynced", "status": "True", "reason": "SyncSucceeded", "message": "Secret synced", "lastTransitionTime": now.UTC().Format(time.RFC3339)}
		status := map[string]any{"observedGeneration": m.Generation, "conditions": []any{cond}}
		if err != nil {
			cond["status"], cond["reason"], cond["message"] = "False", "SyncFailed", err.Error()
			s.env.Log.Info("gke: SecretSync failed", "cluster", key, "secretsync", m.Namespace+"/"+m.Name, "err", err)
		} else {
			status["lastSuccessfulSyncTime"] = now.UTC().Format(time.RFC3339)
		}
		_ = kc.mergePatch(ctx, secretSyncPath+"/namespaces/"+m.Namespace+"/secretsyncs/"+m.Name+"/status", map[string]any{"status": status})
	}
	return nil
}

func (s *Service) syncOne(ctx context.Context, key string, kc *kubeClient, ss *secretSync) error {
	m, spec := ss.Metadata, ss.Spec
	var spc struct {
		Spec struct {
			Provider   string            `json:"provider"`
			Parameters map[string]string `json:"parameters"`
		} `json:"spec"`
	}
	if err := kc.get(ctx, spcPath+"/namespaces/"+m.Namespace+"/secretproviderclasses/"+spec.SecretProviderClassName, &spc); err != nil {
		return fmt.Errorf("SecretProviderClass %s: %w", spec.SecretProviderClassName, err)
	}
	if spc.Spec.Provider != "gke" {
		return fmt.Errorf("SecretProviderClass %s has provider %q, not gke", spec.SecretProviderClassName, spc.Spec.Provider)
	}
	secrets, err := parseSPCSecrets(spc.Spec.Parameters["secrets"])
	if err != nil {
		return err
	}
	rec, err := s.loadKey(key)
	if err != nil {
		return err
	}
	files, err := s.readSecrets(ctx, key, rec.cluster(), m.Namespace, spec.ServiceAccountName, secrets)
	if err != nil {
		return err
	}
	byPath := map[string][]byte{}
	for _, f := range files {
		byPath[f.Path] = f.Contents
	}
	data := map[string][]byte{}
	for _, d := range spec.SecretObject.Data {
		b, ok := byPath[d.SourcePath]
		if !ok {
			return fmt.Errorf("sourcePath %q is not a path of SecretProviderClass %s", d.SourcePath, spec.SecretProviderClassName)
		}
		data[d.TargetKey] = b
	}
	h := sha256.New()
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%x;", k, data[k])
	}
	fmt.Fprintf(h, "type=%s;labels=%v;annotations=%v", spec.SecretObject.Type, spec.SecretObject.Labels, spec.SecretObject.Annotations)
	sum := hex.EncodeToString(h.Sum(nil))

	coll := "/api/v1/namespaces/" + m.Namespace + "/secrets"
	var cur struct {
		Metadata objectMeta `json:"metadata"`
	}
	if err := kc.get(ctx, coll+"/"+m.Name, &cur); err == nil && cur.Metadata.Annotations[hashAnnotation] == sum {
		return nil
	}
	ann := map[string]any{hashAnnotation: sum}
	for k, v := range spec.SecretObject.Annotations {
		ann[k] = v
	}
	labels := map[string]any{}
	for k, v := range spec.SecretObject.Labels {
		labels[k] = v
	}
	typ := spec.SecretObject.Type
	switch typ {
	case "":
		typ = "Opaque"
	case "tls":
		typ = "kubernetes.io/tls"
	case "docker-registry":
		typ = "kubernetes.io/dockerconfigjson"
	}
	obj := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": typ, "data": data,
		"metadata": map[string]any{
			"name": m.Name, "namespace": m.Namespace, "labels": labels, "annotations": ann,
			"ownerReferences": []any{map[string]any{
				"apiVersion": "secret-sync.gke.io/v1", "kind": "SecretSync", "name": m.Name, "uid": m.UID, "controller": true,
			}},
		},
	}
	return applyObject(ctx, kc, coll, m.Name, obj)
}
