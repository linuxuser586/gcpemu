package build

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/linuxuser586/gcpemu/internal/apierr"
	"github.com/linuxuser586/gcpemu/internal/emu"
	"github.com/linuxuser586/gcpemu/internal/project"
)

// The events that fire a Build trigger (FR-CB-013).
//
// A pubsubConfig Build trigger owns the push subscription
// gcb-<Build trigger name> to its topic, made with it and deleted with it. Its
// pushes, to https://cloudbuild.googleapis.com/v1/<Build trigger>:webhook, are
// served in process (emu.Subscriber) and start one Build per message,
// whose substitutions bind to the push body: $(body.message.data), with
// the data decoded (and parsed when it is JSON), and
// $(body.message.attributes.KEY).
//
// A webhookConfig Build trigger is fired by triggers.webhook with the
// payload of its Secret Manager secret version as the secret query
// parameter; its substitutions bind to the request body.

// subscriptionName is the subscription a pubsubConfig Build trigger
// owns.
func subscriptionName(t *cloudbuildpb.BuildTrigger) string {
	r := triggerRefOf(t)
	id := "gcb-" + t.GetName()
	if r.Location != globalLocation {
		id = "gcb-" + r.Location + "-" + t.GetName()
	}
	return "projects/" + r.Project + "/subscriptions/" + id
}

// pushEndpoint is where a pubsubConfig Build trigger's subscription
// pushes.
func pushEndpoint(t *cloudbuildpb.BuildTrigger) string {
	return "https://" + apiHost + "/v1/" + triggerRefOf(t).name() + ":webhook"
}

func (s *Service) subscriber() emu.Subscriber {
	if svc, ok := s.env.Lookup("pubsub"); ok {
		if sub, ok := svc.(emu.Subscriber); ok {
			return sub
		}
	}
	return nil
}

// ensureSubscription makes the subscription of next, a Build trigger
// replacing prev (either may be nil), match its pubsubConfig, deleting
// prev's when it differs.
func (s *Service) ensureSubscription(ctx context.Context, prev, next *cloudbuildpb.BuildTrigger) error {
	var old, want string
	if prev.GetPubsubConfig() != nil {
		old = subscriptionName(prev)
	}
	if next.GetPubsubConfig() != nil {
		want = subscriptionName(next)
	}
	if old == "" && want == "" {
		return nil
	}
	sub := s.subscriber()
	if sub == nil {
		if want == "" {
			return nil
		}
		return apierr.FailedPrecondition("A pubsubConfig Build trigger needs the emulator's Pub/Sub Service (pubsub), which is not running.")
	}
	if want != "" {
		err := sub.EnsurePushSubscription(ctx, want, next.GetPubsubConfig().GetTopic(), pushEndpoint(next))
		if apierr.From(err) != nil && apierr.From(err).Code == codes.NotFound {
			return apierr.InvalidArgument("invalid trigger: pubsubConfig.topic %q does not exist.", next.GetPubsubConfig().GetTopic())
		}
		if err != nil {
			return err
		}
	}
	if old != "" && old != want {
		return sub.DeleteSubscriptionInternal(ctx, old)
	}
	return nil
}

// serveTriggerPush starts a Build for a message pushed to a pubsubConfig
// Build trigger. A message that cannot start one is logged and
// acknowledged, so it is not pushed again; only an error that may pass,
// such as a Reset in progress, has it pushed again.
func (s *Service) serveTriggerPush(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v1/"), ":webhook")
	ref, err := parseTrigger("", "", name)
	if !ok || r.Method != http.MethodPost || err != nil {
		http.NotFound(w, r)
		return
	}
	t, err := s.loadTrigger(ref)
	if err != nil || t.GetPubsubConfig() == nil || t.GetDisabled() {
		return
	}
	var body any
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRESTBody)).Decode(&body); err != nil {
		s.env.Log.Warn("build: Build trigger push is not JSON", "trigger", t.GetResourceName(), "err", err)
		return
	}
	decodeData(body)
	if err := s.fire(r.Context(), t, triggerEvent{body: body}); err != nil {
		switch apierr.From(err).Code {
		case codes.Unavailable, codes.Aborted, codes.Internal:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		s.env.Log.Warn("build: Build trigger did not start a Build", "trigger", t.GetResourceName(), "err", err)
	}
}

// decodeData replaces message.data in a push body with the data decoded,
// and parsed when it is JSON, keeping its text for a binding to the data
// itself.
func decodeData(body any) {
	msg, _ := body.(map[string]any)["message"].(map[string]any)
	data, ok := msg["data"].(string)
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		msg["data"] = jsonText{string(raw), v}
	} else {
		msg["data"] = string(raw)
	}
}

// fire starts a Build of a Build trigger for an event.
func (s *Service) fire(ctx context.Context, t *cloudbuildpb.BuildTrigger, ev triggerEvent) error {
	b, provided, err := s.triggerBuild(ctx, t, ev)
	if err != nil {
		return err
	}
	r := triggerRefOf(t)
	_, err = s.submit(r.Project, r.Location, b, provided)
	return err
}

// ReceiveTriggerWebhook starts a Build of a webhookConfig Build trigger
// when the request's secret is its secret version's payload. It makes no
// IAM check: the secret is the authorization, as with GCP's API key.
func (a *api) ReceiveTriggerWebhook(ctx context.Context, req *cloudbuildpb.ReceiveTriggerWebhookRequest) (*cloudbuildpb.ReceiveTriggerWebhookResponse, error) {
	r, err := parseTrigger(req.GetProjectId(), req.GetTrigger(), req.GetName())
	if err != nil {
		return nil, err
	}
	t, err := a.s.loadTrigger(r)
	if err != nil {
		return nil, err
	}
	if t.GetWebhookConfig() == nil {
		return nil, apierr.FailedPrecondition("Trigger %q is not a webhook trigger.", t.GetResourceName())
	}
	if err := a.s.checkWebhookSecret(ctx, t, req.GetSecret()); err != nil {
		return nil, err
	}
	if t.GetDisabled() {
		return nil, apierr.FailedPrecondition("Trigger %q is disabled.", t.GetResourceName())
	}
	var body any
	if data := req.GetBody().GetData(); len(data) > 0 && json.Unmarshal(data, &body) != nil {
		body = string(data)
	}
	if err := a.s.fire(ctx, t, triggerEvent{body: body}); err != nil {
		return nil, err
	}
	return &cloudbuildpb.ReceiveTriggerWebhookResponse{}, nil
}

// checkWebhookSecret refuses a webhook whose secret is not the payload of
// the Build trigger's secret version, read as the Cloud Build service
// agent.
func (s *Service) checkWebhookSecret(ctx context.Context, t *cloudbuildpb.BuildTrigger, secret string) error {
	svc, _ := s.env.Lookup("secrets")
	acc, ok := svc.(emu.SecretAccessor)
	if !ok {
		return apierr.FailedPrecondition("A webhookConfig Build trigger needs the emulator's Secret Manager Service (secrets), which is not running.")
	}
	agent := "service-" + project.NumberString(triggerRefOf(t).Project) + "@gcp-sa-cloudbuild.iam.gserviceaccount.com"
	want, _, err := acc.AccessSecretVersion(emu.WithPrincipal(ctx, emu.Principal("serviceAccount:"+agent)), t.GetWebhookConfig().GetSecret())
	if err != nil {
		s.env.Log.Warn("build: webhook secret not readable", "trigger", t.GetResourceName(), "secret", t.GetWebhookConfig().GetSecret(), "err", err)
		return apierr.FailedPrecondition("The Build trigger's webhook secret cannot be read.")
	}
	if secret == "" || subtle.ConstantTimeCompare([]byte(secret), want) != 1 {
		return apierr.PermissionDenied("The webhook secret is missing or wrong.")
	}
	return nil
}

// serveWebhook serves triggers.webhook over REST, whose body is the
// webhook's payload, not JSON for the request.
func (s *Service) serveWebhook(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/"), ":webhook")
	req := &cloudbuildpb.ReceiveTriggerWebhookRequest{Secret: r.URL.Query().Get("secret")}
	if segs := strings.Split(rest, "/"); len(segs) == 4 && segs[0] == "projects" && segs[2] == "triggers" {
		req.ProjectId, req.Trigger = segs[1], segs[3]
	} else {
		req.Name = rest
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRESTBody+1))
	if err == nil && len(data) > maxRESTBody {
		err = apierr.InvalidArgument("The request body is larger than %d bytes.", maxRESTBody)
	}
	if err != nil {
		apierr.Write(w, apierr.From(err))
		return
	}
	req.Body = &httpbody.HttpBody{ContentType: r.Header.Get("Content-Type"), Data: data}
	resp, err := s.api.ReceiveTriggerWebhook(r.Context(), req)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	b, _ := protojson.Marshal(resp)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}
