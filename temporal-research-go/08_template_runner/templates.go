package template_runner

// ProvisioningTemplate is a 16-node DAG that mirrors the Python provisioning example.
// Nodes are organised in phases: VPC → subnets → servers/db/cache → lb → dns → health checks.
var ProvisioningTemplate = Template{
	ID:   "provisioning-v1",
	Name: "Environment Provisioning",
	Nodes: []TemplateNode{
		// ── Phase 1: VPC ─────────────────────────────────────────────────────
		{
			ID:       "vpc",
			Name:     "Provision VPC",
			Activity: "ProvisionVPC",
			Params:   map[string]any{},
		},

		// ── Phase 2: Subnets ─────────────────────────────────────────────────
		{
			ID:        "subnet_a",
			Name:      "Provision Subnet A",
			Activity:  "ProvisionSubnet",
			Params:    map[string]any{"vpc_id": "${vpc.vpc_id}", "az": "us-east-1a"},
			DependsOn: []string{"vpc"},
		},
		{
			ID:        "subnet_b",
			Name:      "Provision Subnet B",
			Activity:  "ProvisionSubnet",
			Params:    map[string]any{"vpc_id": "${vpc.vpc_id}", "az": "us-east-1b"},
			DependsOn: []string{"vpc"},
		},
		{
			ID:        "subnet_c",
			Name:      "Provision Subnet C",
			Activity:  "ProvisionSubnet",
			Params:    map[string]any{"vpc_id": "${vpc.vpc_id}", "az": "us-east-1c"},
			DependsOn: []string{"vpc"},
		},

		// ── Phase 3: Servers ──────────────────────────────────────────────────
		{
			ID:        "server_1",
			Name:      "Provision Server 1",
			Activity:  "ProvisionServer",
			Params:    map[string]any{"subnet_id": "${subnet_a.subnet_id}"},
			DependsOn: []string{"subnet_a"},
		},
		{
			ID:        "server_2",
			Name:      "Provision Server 2",
			Activity:  "ProvisionServer",
			Params:    map[string]any{"subnet_id": "${subnet_b.subnet_id}"},
			DependsOn: []string{"subnet_b"},
			Condition: "multi_server",
		},
		{
			ID:        "server_3",
			Name:      "Provision Server 3",
			Activity:  "ProvisionServer",
			Params:    map[string]any{"subnet_id": "${subnet_c.subnet_id}"},
			DependsOn: []string{"subnet_c"},
			Condition: "multi_server",
		},

		// ── Phase 3 (optional): Database & Cache ─────────────────────────────
		{
			ID:        "database",
			Name:      "Provision Database",
			Activity:  "ProvisionDatabase",
			Params:    map[string]any{"vpc_id": "${vpc.vpc_id}"},
			DependsOn: []string{"vpc"},
			Condition: "include_database",
		},
		{
			ID:        "cache",
			Name:      "Provision Cache",
			Activity:  "ProvisionCache",
			Params:    map[string]any{"vpc_id": "${vpc.vpc_id}"},
			DependsOn: []string{"vpc"},
			Condition: "include_cache",
		},

		// ── Phase 4: Load Balancer ────────────────────────────────────────────
		{
			ID:        "load_balancer",
			Name:      "Provision Load Balancer",
			Activity:  "ProvisionLB",
			Params:    map[string]any{"vpc_id": "${vpc.vpc_id}"},
			DependsOn: []string{"server_1", "server_2", "server_3"},
			Condition: "include_load_balancer",
		},

		// ── Phase 5a: DNS ─────────────────────────────────────────────────────
		{
			ID:        "dns",
			Name:      "Register DNS",
			Activity:  "RegisterDNS",
			Params:    map[string]any{"target": "${load_balancer.dns}"},
			DependsOn: []string{"load_balancer"},
			Condition: "include_load_balancer",
		},

		// ── Phase 5b: Health checks ───────────────────────────────────────────
		{
			ID:        "health_server_1",
			Name:      "Health Check Server 1",
			Activity:  "RunHealthCheck",
			Params:    map[string]any{"target": "${server_1.ip}"},
			DependsOn: []string{"server_1"},
		},
		{
			ID:        "health_server_2",
			Name:      "Health Check Server 2",
			Activity:  "RunHealthCheck",
			Params:    map[string]any{"target": "${server_2.ip}"},
			DependsOn: []string{"server_2"},
			Condition: "multi_server",
		},
		{
			ID:        "health_server_3",
			Name:      "Health Check Server 3",
			Activity:  "RunHealthCheck",
			Params:    map[string]any{"target": "${server_3.ip}"},
			DependsOn: []string{"server_3"},
			Condition: "multi_server",
		},
		{
			ID:        "health_db",
			Name:      "Health Check Database",
			Activity:  "RunHealthCheck",
			Params:    map[string]any{"target": "${database.endpoint}"},
			DependsOn: []string{"database"},
			Condition: "include_database",
		},
		{
			ID:        "health_cache",
			Name:      "Health Check Cache",
			Activity:  "RunHealthCheck",
			Params:    map[string]any{"target": "${cache.endpoint}"},
			DependsOn: []string{"cache"},
			Condition: "include_cache",
		},
		{
			ID:        "health_lb",
			Name:      "Health Check Load Balancer",
			Activity:  "RunHealthCheck",
			Params:    map[string]any{"target": "${load_balancer.dns}"},
			DependsOn: []string{"load_balancer"},
			Condition: "include_load_balancer",
		},
	},
}

// DevContext runs a minimal single-server environment without any optional components.
var DevContext = map[string]any{
	"multi_server":          false,
	"include_database":      false,
	"include_cache":         false,
	"include_load_balancer": false,
}

// StagingContext runs a multi-server environment with a database and load balancer but no cache.
var StagingContext = map[string]any{
	"multi_server":          true,
	"include_database":      true,
	"include_cache":         false,
	"include_load_balancer": true,
}

// ProdContext runs the full environment with all optional components enabled.
var ProdContext = map[string]any{
	"multi_server":          true,
	"include_database":      true,
	"include_cache":         true,
	"include_load_balancer": true,
}
