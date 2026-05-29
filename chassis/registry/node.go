package registry

// Node A node that can execute an operation
type Node struct {
	Key     string `json:"key"`
	Address string `json:"address"`
	Port    int    `json:"port"`
}
