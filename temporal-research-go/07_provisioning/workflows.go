package provisioning

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"
)

// ServerWorkflow is a child workflow that provisions a single server.
func ServerWorkflow(ctx workflow.Context, subnetID, az string, idx int) (ServerResult, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)

	var result ServerResult
	err := workflow.ExecuteActivity(actCtx, ProvisionServer, subnetID, az, idx).Get(actCtx, &result)
	if err != nil {
		return ServerResult{}, fmt.Errorf("ProvisionServer failed (idx=%d): %w", idx, err)
	}
	return result, nil
}

// serverChildResult carries the result (or error) from a parallel server child workflow.
type serverChildResult struct {
	result ServerResult
	err    error
}

// subnetResult carries a subnet result from a parallel provisioning goroutine.
type subnetResult struct {
	result SubnetResult
	err    error
}

// EnvironmentWorkflow orchestrates the full multi-phase provisioning of an environment.
func EnvironmentWorkflow(ctx workflow.Context, req ProvisionRequest) (EnvManifest, error) {
	logger := workflow.GetLogger(ctx)

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 60 * time.Second,
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)

	// Phase 1: Provision VPC
	var vpc VPCResult
	if err := workflow.ExecuteActivity(actCtx, ProvisionVPC, req).Get(actCtx, &vpc); err != nil {
		return EnvManifest{}, fmt.Errorf("ProvisionVPC failed: %w", err)
	}
	logger.Info("VPC provisioned", "vpcID", vpc.VPCID, "cidr", vpc.CIDR)

	// Phase 2: Provision 3 subnets in parallel
	azs := []string{"us-east-1a", "us-east-1b", "us-east-1c"}
	subnetCh := workflow.NewBufferedChannel(ctx, len(azs))

	for _, az := range azs {
		az := az
		workflow.Go(ctx, func(gCtx workflow.Context) {
			var sr SubnetResult
			err := workflow.ExecuteActivity(actCtx, ProvisionSubnet, vpc.VPCID, az).Get(actCtx, &sr)
			subnetCh.Send(gCtx, subnetResult{result: sr, err: err})
		})
	}

	subnets := make([]SubnetResult, 0, len(azs))
	for i := 0; i < len(azs); i++ {
		var sr subnetResult
		subnetCh.Receive(ctx, &sr)
		if sr.err != nil {
			return EnvManifest{}, fmt.Errorf("ProvisionSubnet failed: %w", sr.err)
		}
		subnets = append(subnets, sr.result)
		logger.Info("Subnet provisioned", "subnetID", sr.result.SubnetID, "az", sr.result.AZ)
	}

	// Phase 3: Servers + optional DB + optional Cache in parallel
	optCount := 0
	if req.IncludeDB {
		optCount++
	}
	if req.IncludeCache {
		optCount++
	}

	serverCh := workflow.NewBufferedChannel(ctx, req.ServerCount)

	type optionalResult struct {
		db    *DatabaseResult
		cache *CacheResult
		err   error
	}
	optCh := workflow.NewBufferedChannel(ctx, optCount+1) // +1 to avoid zero-size

	// Launch server child workflows.
	for i := 0; i < req.ServerCount; i++ {
		idx := i
		subnet := subnets[idx%len(subnets)]
		workflow.Go(ctx, func(gCtx workflow.Context) {
			cwo := workflow.ChildWorkflowOptions{
				WorkflowExecutionTimeout: 60 * time.Second,
			}
			childCtx := workflow.WithChildOptions(gCtx, cwo)
			var sr ServerResult
			err := workflow.ExecuteChildWorkflow(childCtx, ServerWorkflow, subnet.SubnetID, subnet.AZ, idx+1).Get(childCtx, &sr)
			serverCh.Send(gCtx, serverChildResult{result: sr, err: err})
		})
	}

	// Optionally launch database provisioning.
	if req.IncludeDB {
		workflow.Go(ctx, func(gCtx workflow.Context) {
			var dr DatabaseResult
			err := workflow.ExecuteActivity(actCtx, ProvisionDatabase, vpc.VPCID).Get(actCtx, &dr)
			if err != nil {
				optCh.Send(gCtx, optionalResult{err: fmt.Errorf("ProvisionDatabase: %w", err)})
				return
			}
			optCh.Send(gCtx, optionalResult{db: &dr})
		})
	}

	// Optionally launch cache provisioning.
	if req.IncludeCache {
		workflow.Go(ctx, func(gCtx workflow.Context) {
			var cr CacheResult
			err := workflow.ExecuteActivity(actCtx, ProvisionCache, vpc.VPCID).Get(actCtx, &cr)
			if err != nil {
				optCh.Send(gCtx, optionalResult{err: fmt.Errorf("ProvisionCache: %w", err)})
				return
			}
			optCh.Send(gCtx, optionalResult{cache: &cr})
		})
	}

	// Collect server results.
	servers := make([]ServerResult, 0, req.ServerCount)
	for i := 0; i < req.ServerCount; i++ {
		var sr serverChildResult
		serverCh.Receive(ctx, &sr)
		if sr.err != nil {
			return EnvManifest{}, fmt.Errorf("ServerWorkflow failed: %w", sr.err)
		}
		servers = append(servers, sr.result)
		logger.Info("Server provisioned", "serverID", sr.result.ServerID, "ip", sr.result.IP)
	}

	// Collect optional results.
	var dbResult *DatabaseResult
	var cacheResult *CacheResult
	for i := 0; i < optCount; i++ {
		var or optionalResult
		optCh.Receive(ctx, &or)
		if or.err != nil {
			return EnvManifest{}, or.err
		}
		if or.db != nil {
			dbResult = or.db
			logger.Info("Database provisioned", "endpoint", or.db.Endpoint)
		}
		if or.cache != nil {
			cacheResult = or.cache
			logger.Info("Cache provisioned", "endpoint", or.cache.Endpoint)
		}
	}

	// Phase 4: Optional Load Balancer
	var lbResult *LBResult
	if req.IncludeLB {
		serverIDs := make([]string, len(servers))
		for i, s := range servers {
			serverIDs[i] = s.ServerID
		}
		var lb LBResult
		if err := workflow.ExecuteActivity(actCtx, ProvisionLoadBalancer, vpc.VPCID, serverIDs).Get(actCtx, &lb); err != nil {
			return EnvManifest{}, fmt.Errorf("ProvisionLoadBalancer failed: %w", err)
		}
		lbResult = &lb
		logger.Info("Load balancer provisioned", "dns", lb.DNS)
	}

	// Phase 5: DNS registration + parallel health checks
	var dnsRecord *DNSRecord
	if lbResult != nil {
		var dns DNSRecord
		if err := workflow.ExecuteActivity(actCtx, RegisterDNS, req.Env, lbResult.DNS).Get(actCtx, &dns); err != nil {
			return EnvManifest{}, fmt.Errorf("RegisterDNS failed: %w", err)
		}
		dnsRecord = &dns
		logger.Info("DNS registered", "name", dns.Name, "target", dns.Target)
	}

	// Health-check each server in parallel.
	hcCh := workflow.NewBufferedChannel(ctx, len(servers))
	for _, srv := range servers {
		ip := srv.IP
		workflow.Go(ctx, func(gCtx workflow.Context) {
			var ok bool
			err := workflow.ExecuteActivity(actCtx, RunHealthCheck, ip).Get(actCtx, &ok)
			if err != nil {
				logger.Info("Health check failed", "ip", ip, "error", err)
			} else {
				logger.Info("Health check passed", "ip", ip, "ok", ok)
			}
			hcCh.Send(gCtx, struct{}{})
		})
	}
	for i := 0; i < len(servers); i++ {
		hcCh.Receive(ctx, nil)
	}

	return EnvManifest{
		Env:     req.Env,
		VPC:     vpc,
		Subnets: subnets,
		Servers: servers,
		DB:      dbResult,
		Cache:   cacheResult,
		LB:      lbResult,
		DNS:     dnsRecord,
	}, nil
}
