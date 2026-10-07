package certs

// Code generated from the gRPC service definitions by a one-off script;
// each method adapts its request to the resource engine (engine.go).

import (
	"context"

	"cloud.google.com/go/certificatemanager/apiv1/certificatemanagerpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"

	"github.com/linuxuser586/gcpemu/internal/apierr"
)

// cmServer implements google.cloud.certificatemanager.v1.CertificateManager.
type cmServer struct {
	certificatemanagerpb.UnimplementedCertificateManagerServer
	s *Service
}

// nsServer implements the backend authentication config and TLS policy
// methods of google.cloud.networksecurity.v1.NetworkSecurity; the other
// resources of that service return UNIMPLEMENTED.
type nsServer struct {
	networksecuritypb.UnimplementedNetworkSecurityServer
	s *Service
}

func (g *cmServer) ListCertificates(ctx context.Context, req *certificatemanagerpb.ListCertificatesRequest) (*certificatemanagerpb.ListCertificatesResponse, error) {
	res, err := g.s.list(ctx, kCert, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	out := &certificatemanagerpb.ListCertificatesResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(certificatemanagerpb.Certificate)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.Certificates = append(out.Certificates, m)
	}
	return out, nil
}

func (g *cmServer) GetCertificate(ctx context.Context, req *certificatemanagerpb.GetCertificateRequest) (*certificatemanagerpb.Certificate, error) {
	o, err := g.s.get(ctx, kCert, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(certificatemanagerpb.Certificate)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *cmServer) CreateCertificate(ctx context.Context, req *certificatemanagerpb.CreateCertificateRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kCert, req.GetCertificate())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kCert, req.GetParent(), req.GetCertificateId(), in)
}

func (g *cmServer) UpdateCertificate(ctx context.Context, req *certificatemanagerpb.UpdateCertificateRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kCert, req.GetCertificate())
	if err != nil {
		return nil, err
	}
	return g.s.patch(ctx, kCert, req.GetCertificate().GetName(), in, req.GetUpdateMask().GetPaths())
}

func (g *cmServer) DeleteCertificate(ctx context.Context, req *certificatemanagerpb.DeleteCertificateRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kCert, req.GetName(), "")
}

func (g *cmServer) ListCertificateMaps(ctx context.Context, req *certificatemanagerpb.ListCertificateMapsRequest) (*certificatemanagerpb.ListCertificateMapsResponse, error) {
	res, err := g.s.list(ctx, kMap, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	out := &certificatemanagerpb.ListCertificateMapsResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(certificatemanagerpb.CertificateMap)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.CertificateMaps = append(out.CertificateMaps, m)
	}
	return out, nil
}

func (g *cmServer) GetCertificateMap(ctx context.Context, req *certificatemanagerpb.GetCertificateMapRequest) (*certificatemanagerpb.CertificateMap, error) {
	o, err := g.s.get(ctx, kMap, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(certificatemanagerpb.CertificateMap)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *cmServer) CreateCertificateMap(ctx context.Context, req *certificatemanagerpb.CreateCertificateMapRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kMap, req.GetCertificateMap())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kMap, req.GetParent(), req.GetCertificateMapId(), in)
}

func (g *cmServer) UpdateCertificateMap(ctx context.Context, req *certificatemanagerpb.UpdateCertificateMapRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kMap, req.GetCertificateMap())
	if err != nil {
		return nil, err
	}
	return g.s.patch(ctx, kMap, req.GetCertificateMap().GetName(), in, req.GetUpdateMask().GetPaths())
}

func (g *cmServer) DeleteCertificateMap(ctx context.Context, req *certificatemanagerpb.DeleteCertificateMapRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kMap, req.GetName(), "")
}

func (g *cmServer) ListCertificateMapEntries(ctx context.Context, req *certificatemanagerpb.ListCertificateMapEntriesRequest) (*certificatemanagerpb.ListCertificateMapEntriesResponse, error) {
	res, err := g.s.list(ctx, kEntry, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	out := &certificatemanagerpb.ListCertificateMapEntriesResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(certificatemanagerpb.CertificateMapEntry)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.CertificateMapEntries = append(out.CertificateMapEntries, m)
	}
	return out, nil
}

func (g *cmServer) GetCertificateMapEntry(ctx context.Context, req *certificatemanagerpb.GetCertificateMapEntryRequest) (*certificatemanagerpb.CertificateMapEntry, error) {
	o, err := g.s.get(ctx, kEntry, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(certificatemanagerpb.CertificateMapEntry)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *cmServer) CreateCertificateMapEntry(ctx context.Context, req *certificatemanagerpb.CreateCertificateMapEntryRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kEntry, req.GetCertificateMapEntry())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kEntry, req.GetParent(), req.GetCertificateMapEntryId(), in)
}

func (g *cmServer) UpdateCertificateMapEntry(ctx context.Context, req *certificatemanagerpb.UpdateCertificateMapEntryRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kEntry, req.GetCertificateMapEntry())
	if err != nil {
		return nil, err
	}
	return g.s.patch(ctx, kEntry, req.GetCertificateMapEntry().GetName(), in, req.GetUpdateMask().GetPaths())
}

func (g *cmServer) DeleteCertificateMapEntry(ctx context.Context, req *certificatemanagerpb.DeleteCertificateMapEntryRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kEntry, req.GetName(), "")
}

func (g *cmServer) ListDnsAuthorizations(ctx context.Context, req *certificatemanagerpb.ListDnsAuthorizationsRequest) (*certificatemanagerpb.ListDnsAuthorizationsResponse, error) {
	res, err := g.s.list(ctx, kDNSAuth, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	out := &certificatemanagerpb.ListDnsAuthorizationsResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(certificatemanagerpb.DnsAuthorization)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.DnsAuthorizations = append(out.DnsAuthorizations, m)
	}
	return out, nil
}

func (g *cmServer) GetDnsAuthorization(ctx context.Context, req *certificatemanagerpb.GetDnsAuthorizationRequest) (*certificatemanagerpb.DnsAuthorization, error) {
	o, err := g.s.get(ctx, kDNSAuth, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(certificatemanagerpb.DnsAuthorization)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *cmServer) CreateDnsAuthorization(ctx context.Context, req *certificatemanagerpb.CreateDnsAuthorizationRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kDNSAuth, req.GetDnsAuthorization())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kDNSAuth, req.GetParent(), req.GetDnsAuthorizationId(), in)
}

func (g *cmServer) UpdateDnsAuthorization(ctx context.Context, req *certificatemanagerpb.UpdateDnsAuthorizationRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kDNSAuth, req.GetDnsAuthorization())
	if err != nil {
		return nil, err
	}
	return g.s.patch(ctx, kDNSAuth, req.GetDnsAuthorization().GetName(), in, req.GetUpdateMask().GetPaths())
}

func (g *cmServer) DeleteDnsAuthorization(ctx context.Context, req *certificatemanagerpb.DeleteDnsAuthorizationRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kDNSAuth, req.GetName(), "")
}

func (g *cmServer) ListCertificateIssuanceConfigs(ctx context.Context, req *certificatemanagerpb.ListCertificateIssuanceConfigsRequest) (*certificatemanagerpb.ListCertificateIssuanceConfigsResponse, error) {
	res, err := g.s.list(ctx, kIssuance, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	out := &certificatemanagerpb.ListCertificateIssuanceConfigsResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(certificatemanagerpb.CertificateIssuanceConfig)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.CertificateIssuanceConfigs = append(out.CertificateIssuanceConfigs, m)
	}
	return out, nil
}

func (g *cmServer) GetCertificateIssuanceConfig(ctx context.Context, req *certificatemanagerpb.GetCertificateIssuanceConfigRequest) (*certificatemanagerpb.CertificateIssuanceConfig, error) {
	o, err := g.s.get(ctx, kIssuance, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(certificatemanagerpb.CertificateIssuanceConfig)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *cmServer) CreateCertificateIssuanceConfig(ctx context.Context, req *certificatemanagerpb.CreateCertificateIssuanceConfigRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kIssuance, req.GetCertificateIssuanceConfig())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kIssuance, req.GetParent(), req.GetCertificateIssuanceConfigId(), in)
}

func (g *cmServer) DeleteCertificateIssuanceConfig(ctx context.Context, req *certificatemanagerpb.DeleteCertificateIssuanceConfigRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kIssuance, req.GetName(), "")
}

func (g *cmServer) ListTrustConfigs(ctx context.Context, req *certificatemanagerpb.ListTrustConfigsRequest) (*certificatemanagerpb.ListTrustConfigsResponse, error) {
	res, err := g.s.list(ctx, kTrust, req.GetParent(), req.GetPageSize(), req.GetPageToken(), req.GetFilter(), req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	out := &certificatemanagerpb.ListTrustConfigsResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(certificatemanagerpb.TrustConfig)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.TrustConfigs = append(out.TrustConfigs, m)
	}
	return out, nil
}

func (g *cmServer) GetTrustConfig(ctx context.Context, req *certificatemanagerpb.GetTrustConfigRequest) (*certificatemanagerpb.TrustConfig, error) {
	o, err := g.s.get(ctx, kTrust, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(certificatemanagerpb.TrustConfig)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *cmServer) CreateTrustConfig(ctx context.Context, req *certificatemanagerpb.CreateTrustConfigRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kTrust, req.GetTrustConfig())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kTrust, req.GetParent(), req.GetTrustConfigId(), in)
}

func (g *cmServer) UpdateTrustConfig(ctx context.Context, req *certificatemanagerpb.UpdateTrustConfigRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kTrust, req.GetTrustConfig())
	if err != nil {
		return nil, err
	}
	return g.s.patch(ctx, kTrust, req.GetTrustConfig().GetName(), in, req.GetUpdateMask().GetPaths())
}

func (g *cmServer) DeleteTrustConfig(ctx context.Context, req *certificatemanagerpb.DeleteTrustConfigRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kTrust, req.GetName(), req.GetEtag())
}

func (g *nsServer) ListBackendAuthenticationConfigs(ctx context.Context, req *networksecuritypb.ListBackendAuthenticationConfigsRequest) (*networksecuritypb.ListBackendAuthenticationConfigsResponse, error) {
	res, err := g.s.list(ctx, kBAC, req.GetParent(), req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, err
	}
	out := &networksecuritypb.ListBackendAuthenticationConfigsResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(networksecuritypb.BackendAuthenticationConfig)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.BackendAuthenticationConfigs = append(out.BackendAuthenticationConfigs, m)
	}
	return out, nil
}

func (g *nsServer) GetBackendAuthenticationConfig(ctx context.Context, req *networksecuritypb.GetBackendAuthenticationConfigRequest) (*networksecuritypb.BackendAuthenticationConfig, error) {
	o, err := g.s.get(ctx, kBAC, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(networksecuritypb.BackendAuthenticationConfig)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *nsServer) CreateBackendAuthenticationConfig(ctx context.Context, req *networksecuritypb.CreateBackendAuthenticationConfigRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kBAC, req.GetBackendAuthenticationConfig())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kBAC, req.GetParent(), req.GetBackendAuthenticationConfigId(), in)
}

func (g *nsServer) UpdateBackendAuthenticationConfig(ctx context.Context, req *networksecuritypb.UpdateBackendAuthenticationConfigRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kBAC, req.GetBackendAuthenticationConfig())
	if err != nil {
		return nil, err
	}
	return g.s.patch(ctx, kBAC, req.GetBackendAuthenticationConfig().GetName(), in, req.GetUpdateMask().GetPaths())
}

func (g *nsServer) DeleteBackendAuthenticationConfig(ctx context.Context, req *networksecuritypb.DeleteBackendAuthenticationConfigRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kBAC, req.GetName(), req.GetEtag())
}

func (g *nsServer) ListServerTlsPolicies(ctx context.Context, req *networksecuritypb.ListServerTlsPoliciesRequest) (*networksecuritypb.ListServerTlsPoliciesResponse, error) {
	res, err := g.s.list(ctx, kServerTLS, req.GetParent(), req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, err
	}
	out := &networksecuritypb.ListServerTlsPoliciesResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(networksecuritypb.ServerTlsPolicy)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.ServerTlsPolicies = append(out.ServerTlsPolicies, m)
	}
	return out, nil
}

func (g *nsServer) GetServerTlsPolicy(ctx context.Context, req *networksecuritypb.GetServerTlsPolicyRequest) (*networksecuritypb.ServerTlsPolicy, error) {
	o, err := g.s.get(ctx, kServerTLS, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(networksecuritypb.ServerTlsPolicy)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *nsServer) CreateServerTlsPolicy(ctx context.Context, req *networksecuritypb.CreateServerTlsPolicyRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kServerTLS, req.GetServerTlsPolicy())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kServerTLS, req.GetParent(), req.GetServerTlsPolicyId(), in)
}

func (g *nsServer) UpdateServerTlsPolicy(ctx context.Context, req *networksecuritypb.UpdateServerTlsPolicyRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kServerTLS, req.GetServerTlsPolicy())
	if err != nil {
		return nil, err
	}
	return g.s.patch(ctx, kServerTLS, req.GetServerTlsPolicy().GetName(), in, req.GetUpdateMask().GetPaths())
}

func (g *nsServer) DeleteServerTlsPolicy(ctx context.Context, req *networksecuritypb.DeleteServerTlsPolicyRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kServerTLS, req.GetName(), "")
}

func (g *nsServer) ListClientTlsPolicies(ctx context.Context, req *networksecuritypb.ListClientTlsPoliciesRequest) (*networksecuritypb.ListClientTlsPoliciesResponse, error) {
	res, err := g.s.list(ctx, kClientTLS, req.GetParent(), req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, err
	}
	out := &networksecuritypb.ListClientTlsPoliciesResponse{NextPageToken: res.next}
	for _, o := range res.items {
		m := new(networksecuritypb.ClientTlsPolicy)
		if err := toProto(o, m); err != nil {
			return nil, apierr.Internal("encode: %v", err)
		}
		out.ClientTlsPolicies = append(out.ClientTlsPolicies, m)
	}
	return out, nil
}

func (g *nsServer) GetClientTlsPolicy(ctx context.Context, req *networksecuritypb.GetClientTlsPolicyRequest) (*networksecuritypb.ClientTlsPolicy, error) {
	o, err := g.s.get(ctx, kClientTLS, req.GetName())
	if err != nil {
		return nil, err
	}
	m := new(networksecuritypb.ClientTlsPolicy)
	if err := toProto(o, m); err != nil {
		return nil, apierr.Internal("encode: %v", err)
	}
	return m, nil
}

func (g *nsServer) CreateClientTlsPolicy(ctx context.Context, req *networksecuritypb.CreateClientTlsPolicyRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kClientTLS, req.GetClientTlsPolicy())
	if err != nil {
		return nil, err
	}
	return g.s.create(ctx, kClientTLS, req.GetParent(), req.GetClientTlsPolicyId(), in)
}

func (g *nsServer) UpdateClientTlsPolicy(ctx context.Context, req *networksecuritypb.UpdateClientTlsPolicyRequest) (*longrunningpb.Operation, error) {
	in, err := fromProto(kClientTLS, req.GetClientTlsPolicy())
	if err != nil {
		return nil, err
	}
	return g.s.patch(ctx, kClientTLS, req.GetClientTlsPolicy().GetName(), in, req.GetUpdateMask().GetPaths())
}

func (g *nsServer) DeleteClientTlsPolicy(ctx context.Context, req *networksecuritypb.DeleteClientTlsPolicyRequest) (*longrunningpb.Operation, error) {
	return g.s.remove(ctx, kClientTLS, req.GetName(), "")
}
