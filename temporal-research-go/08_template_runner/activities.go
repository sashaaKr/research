package template_runner

import (
	"context"
	"fmt"
	"time"
)

// fakeID generates a short fake identifier using the current millisecond clock.
func fakeID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixMilli()%10000)
}

// ProvisionVPC simulates VPC provisioning.
func ProvisionVPC(ctx context.Context, params map[string]any) (map[string]any, error) {
	time.Sleep(400 * time.Millisecond)
	return map[string]any{
		"vpc_id": fakeID("vpc"),
		"cidr":   "10.0.0.0/16",
	}, nil
}

// ProvisionSubnet simulates subnet provisioning.
func ProvisionSubnet(ctx context.Context, params map[string]any) (map[string]any, error) {
	time.Sleep(250 * time.Millisecond)
	return map[string]any{
		"subnet_id": fakeID("subnet"),
		"az":        "us-east-1a",
	}, nil
}

// ProvisionServer simulates server provisioning.
func ProvisionServer(ctx context.Context, params map[string]any) (map[string]any, error) {
	time.Sleep(700 * time.Millisecond)
	return map[string]any{
		"server_id": fakeID("srv"),
		"ip":        fmt.Sprintf("10.0.1.%d", time.Now().UnixMilli()%200+10),
	}, nil
}

// ProvisionDatabase simulates database provisioning.
func ProvisionDatabase(ctx context.Context, params map[string]any) (map[string]any, error) {
	time.Sleep(1000 * time.Millisecond)
	return map[string]any{
		"endpoint": "db.local",
		"port":     5432,
	}, nil
}

// ProvisionCache simulates cache provisioning.
func ProvisionCache(ctx context.Context, params map[string]any) (map[string]any, error) {
	time.Sleep(500 * time.Millisecond)
	return map[string]any{
		"endpoint": "cache.local",
		"port":     6379,
	}, nil
}

// ProvisionLB simulates load-balancer provisioning.
func ProvisionLB(ctx context.Context, params map[string]any) (map[string]any, error) {
	time.Sleep(350 * time.Millisecond)
	return map[string]any{
		"dns": "lb.example.com",
	}, nil
}

// RegisterDNS simulates DNS record creation.
func RegisterDNS(ctx context.Context, params map[string]any) (map[string]any, error) {
	time.Sleep(200 * time.Millisecond)
	return map[string]any{
		"record": "app.example.com",
		"target": "lb.example.com",
	}, nil
}

// RunHealthCheck simulates a health check against a target.
func RunHealthCheck(ctx context.Context, params map[string]any) (map[string]any, error) {
	time.Sleep(300 * time.Millisecond)
	return map[string]any{
		"healthy":    true,
		"latency_ms": 12,
	}, nil
}
