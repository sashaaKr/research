package provisioning

// ProvisionRequest describes what environment to provision.
type ProvisionRequest struct {
	Env          string `json:"env"`
	Region       string `json:"region"`
	ServerCount  int    `json:"server_count"`
	IncludeDB    bool   `json:"include_db"`
	IncludeCache bool   `json:"include_cache"`
	IncludeLB    bool   `json:"include_lb"`
}

// VPCResult holds the details of a provisioned VPC.
type VPCResult struct {
	VPCID string `json:"vpc_id"`
	CIDR  string `json:"cidr"`
}

// SubnetResult holds the details of a provisioned subnet.
type SubnetResult struct {
	SubnetID string `json:"subnet_id"`
	AZ       string `json:"az"`
}

// ServerResult holds the details of a provisioned server.
type ServerResult struct {
	ServerID string `json:"server_id"`
	IP       string `json:"ip"`
}

// DatabaseResult holds the details of a provisioned database.
type DatabaseResult struct {
	Endpoint string `json:"endpoint"`
	Port     int    `json:"port"`
}

// CacheResult holds the details of a provisioned cache.
type CacheResult struct {
	Endpoint string `json:"endpoint"`
	Port     int    `json:"port"`
}

// LBResult holds the details of a provisioned load balancer.
type LBResult struct {
	DNS string `json:"dns"`
}

// DNSRecord holds the details of a registered DNS record.
type DNSRecord struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

// EnvManifest is the final output of a successful environment provisioning run.
type EnvManifest struct {
	Env     string         `json:"env"`
	VPC     VPCResult      `json:"vpc"`
	Subnets []SubnetResult `json:"subnets"`
	Servers []ServerResult `json:"servers"`
	DB      *DatabaseResult `json:"db,omitempty"`
	Cache   *CacheResult   `json:"cache,omitempty"`
	LB      *LBResult      `json:"lb,omitempty"`
	DNS     *DNSRecord     `json:"dns,omitempty"`
}
