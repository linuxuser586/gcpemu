package compute

// routes registers the compute v1 REST surface.
func (s *Service) routes() {
	const p = "/compute/v1/projects/{project}"
	m := s.mux
	h := m.HandleFunc

	h("GET "+p, s.getProject)
	h("GET "+p+"/regions", s.listRegions)
	h("GET "+p+"/regions/{region}", s.getRegion)
	h("GET "+p+"/zones", s.listZones)
	h("GET "+p+"/zones/{zone}", s.getZone)

	// Operations.
	for _, sc := range []string{p + "/global", p + "/regions/{region}", p + "/zones/{zone}"} {
		h("GET "+sc+"/operations", s.listOperations)
		h("GET "+sc+"/operations/{operation}", s.getOperation)
		h("DELETE "+sc+"/operations/{operation}", s.deleteOperation)
		h("POST "+sc+"/operations/{operation}/wait", s.waitOperation)
	}
	h("GET "+p+"/aggregated/operations", s.aggregatedOperations)

	// Networks.
	n := p + "/global/networks"
	h("POST "+n, s.insertNetwork)
	h("GET "+n, s.listNetworks)
	h("GET "+n+"/{network}", s.getNetwork)
	h("PATCH "+n+"/{network}", s.patchNetwork)
	h("DELETE "+n+"/{network}", s.deleteNetwork)
	h("POST "+n+"/{network}/switchToCustomMode", s.switchToCustomMode)
	h("POST "+n+"/{network}/addPeering", s.addPeering)
	h("PATCH "+n+"/{network}/updatePeering", s.updatePeering)
	h("POST "+n+"/{network}/removePeering", s.removePeering)

	// Subnetworks.
	sn := p + "/regions/{region}/subnetworks"
	h("POST "+sn, s.insertSubnetwork)
	h("GET "+sn, s.listSubnetworks)
	h("GET "+sn+"/{subnetwork}", s.getSubnetwork)
	h("PATCH "+sn+"/{subnetwork}", s.patchSubnetwork)
	h("DELETE "+sn+"/{subnetwork}", s.deleteSubnetwork)
	h("POST "+sn+"/{subnetwork}/expandIpCidrRange", s.expandIpCidrRange)
	h("POST "+sn+"/{subnetwork}/setPrivateIpGoogleAccess", s.setPrivateIpGoogleAccess)
	h("GET "+p+"/aggregated/subnetworks", s.aggregatedSubnetworks)

	// Firewalls and routes.
	fw := p + "/global/firewalls"
	h("POST "+fw, s.insertFirewall)
	h("GET "+fw, s.listFirewalls)
	h("GET "+fw+"/{firewall}", s.getFirewall)
	h("PATCH "+fw+"/{firewall}", s.updateFirewall)
	h("PUT "+fw+"/{firewall}", s.updateFirewall)
	h("DELETE "+fw+"/{firewall}", s.deleteFirewall)
	rt := p + "/global/routes"
	h("POST "+rt, s.insertRoute)
	h("GET "+rt, s.listRoutes)
	h("GET "+rt+"/{route}", s.getRoute)
	h("DELETE "+rt+"/{route}", s.deleteRoute)

	// Addresses (regional and global).
	for _, a := range []string{p + "/regions/{region}/addresses", p + "/global/addresses"} {
		h("POST "+a, s.insertAddress)
		h("GET "+a, s.listAddresses)
		h("GET "+a+"/{address}", s.getAddress)
		h("DELETE "+a+"/{address}", s.deleteAddress)
		h("POST "+a+"/{address}/setLabels", s.setAddressLabels)
	}
	h("GET "+p+"/aggregated/addresses", s.aggregatedAddresses)

	// Routers and Cloud NAT.
	ro := p + "/regions/{region}/routers"
	h("POST "+ro, s.insertRouter)
	h("GET "+ro, s.listRouters)
	h("GET "+ro+"/{router}", s.getRouter)
	h("PATCH "+ro+"/{router}", s.updateRouter)
	h("PUT "+ro+"/{router}", s.updateRouter)
	h("DELETE "+ro+"/{router}", s.deleteRouter)
	h("GET "+ro+"/{router}/getRouterStatus", s.getRouterStatus)
	h("GET "+ro+"/{router}/getNatMappingInfo", s.getNatMappingInfo)
	h("GET "+p+"/aggregated/routers", s.aggregatedRouters)

	// Network endpoint groups (zonal, regional and global).
	for _, ng := range []string{p + "/zones/{zone}/networkEndpointGroups", p + "/regions/{region}/networkEndpointGroups", p + "/global/networkEndpointGroups"} {
		h("POST "+ng, s.insertNEG)
		h("GET "+ng, s.listNEGs)
		h("GET "+ng+"/{neg}", s.getNEG)
		h("DELETE "+ng+"/{neg}", s.deleteNEG)
		h("POST "+ng+"/{neg}/attachNetworkEndpoints", s.attachEndpoints)
		h("POST "+ng+"/{neg}/detachNetworkEndpoints", s.detachEndpoints)
		h("POST "+ng+"/{neg}/listNetworkEndpoints", s.listNetworkEndpoints)
	}
	h("GET "+p+"/aggregated/networkEndpointGroups", s.aggregatedNEGs)

	h("POST "+natEventsPath, s.natEvents)
	h("/", notFoundHandler)
}
