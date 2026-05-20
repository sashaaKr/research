package template_runner

// NodeStatus represents the lifecycle state of a single template node.
type NodeStatus string

const (
	Pending   NodeStatus = "PENDING"
	Running   NodeStatus = "RUNNING"
	Completed NodeStatus = "COMPLETED"
	Skipped   NodeStatus = "SKIPPED"
	Failed    NodeStatus = "FAILED"
)

// TemplateNode describes a single unit of work within a Template DAG.
type TemplateNode struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Activity  string         `json:"activity"`   // registered activity name
	Params    map[string]any `json:"params"`     // supports "${ctx.field}" and "${node_id.field}" refs
	DependsOn []string       `json:"depends_on"` // IDs of nodes that must complete first
	Condition string         `json:"condition"`  // key in input_context; skip node if false/missing
}

// Template is a DAG of TemplateNodes that can be executed by TemplateRunnerWorkflow.
type Template struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Nodes []TemplateNode `json:"nodes"`
}

// NodeState tracks the runtime state of a single node during execution.
type NodeState struct {
	NodeID     string     `json:"node_id"`
	NodeName   string     `json:"node_name"`
	Condition  string     `json:"condition,omitempty"`
	Status     NodeStatus `json:"status"`
	Output     any        `json:"output,omitempty"`
	Error      string     `json:"error,omitempty"`
	SkipReason string     `json:"skip_reason,omitempty"`
}

// ExecutionGraph is a snapshot of every node's state during or after execution.
type ExecutionGraph struct {
	TemplateID string               `json:"template_id"`
	Nodes      map[string]NodeState `json:"nodes"`
}
