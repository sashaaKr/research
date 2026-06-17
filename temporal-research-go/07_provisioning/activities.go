package provisioning

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ProvisionVPC provisions a VPC and returns its ID and CIDR block.
func ProvisionVPC(ctx context.Context, req ProvisionRequest) (VPCResult, error) {
	time.Sleep(500 * time.Millisecond)
	return VPCResult{
		VPCID: fmt.Sprintf("vpc-%s", req.Env),
		CIDR:  "10.0.0.0/16",
	}, nil
}

// ProvisionSubnet provisions a subnet within the given VPC in the given availability zone.
func ProvisionSubnet(ctx context.Context, vpcID, az string) (SubnetResult, error) {
	time.Sleep(300 * time.Millisecond)
	// Derive a short suffix from the AZ, e.g. "us-east-1a" → "1a"
	parts := strings.Split(az, "-")
	suffix := parts[len(parts)-1]
	return SubnetResult{
		SubnetID: fmt.Sprintf("subnet-%s-%s", vpcID, suffix),
		AZ:       az,
	}, nil
}

// ProvisionServer provisions a server in the given subnet.
func ProvisionServer(ctx context.Context, subnetID, az string, idx int) (ServerResult, error) {
	time.Sleep(800 * time.Millisecond)
	return ServerResult{
		ServerID: fmt.Sprintf("srv-%s-%d", subnetID, idx),
		IP:       fmt.Sprintf("10.0.%d.%d", idx, 10+idx),
	}, nil
}

// ProvisionDatabase provisions a managed database in the given VPC.
func ProvisionDatabase(ctx context.Context, vpcID string) (DatabaseResult, error) {
	time.Sleep(1200 * time.Millisecond)
	return DatabaseResult{
		Endpoint: fmt.Sprintf("db.%s.internal", vpcID),
		Port:     5432,
	}, nil
}

// ProvisionCache provisions an in-memory cache cluster in the given VPC.
func ProvisionCache(ctx context.Context, vpcID string) (CacheResult, error) {
	time.Sleep(600 * time.Millisecond)
	return CacheResult{
		Endpoint: fmt.Sprintf("cache.%s.internal", vpcID),
		Port:     6379,
	}, nil
}

// ProvisionLoadBalancer provisions a load balancer targeting the given server IDs.
func ProvisionLoadBalancer(ctx context.Context, vpcID string, serverIDs []string) (LBResult, error) {
	time.Sleep(400 * time.Millisecond)
	return LBResult{
		DNS: fmt.Sprintf("lb.%s.example.com", vpcID),
	}, nil
}

// RegisterDNS creates a DNS record pointing the environment's hostname to the target.
func RegisterDNS(ctx context.Context, env, target string) (DNSRecord, error) {
	time.Sleep(200 * time.Millisecond)
	return DNSRecord{
		Name:   fmt.Sprintf("%s.example.com", env),
		Target: target,
	}, nil
}

// RunHealthCheck verifies that the given target is healthy.
func RunHealthCheck(ctx context.Context, target string) (bool, error) {
	time.Sleep(300 * time.Millisecond)
	return true, nil
}
