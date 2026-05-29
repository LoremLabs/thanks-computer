package registry

import (
	"math/rand"
	"regexp"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/operation"
)

// DefaultPort Default port for op service
const DefaultPort = 5858

// FixedNodes Get the fixed nodes that have been configured
func (reg *Registry) FixedNodes() ([]*Node, error) {
	var nodes []*Node

	nodes = make([]*Node, 0)

	for _, serviceDef := range reg.Conf.RegistryFixed {
		// split into key, address, port
		s := strings.Split(serviceDef, ":")
		if len(s) != 3 {
			// so sorry we were looking for key:address:port
			continue
		}

		port, err := strconv.Atoi(s[2])
		if err != nil {
			return nil, err
		}

		node := &Node{Key: s[0], Address: s[1], Port: port}
		nodes = append(nodes, node)
	}

	return nodes, nil
}

// NodeByFixed For a given operation, find best non-ring node
func (reg *Registry) NodeByFixed(op operation.Operation) (*Node, error) {

	// get any fixed nodes
	nodes, err := reg.FixedNodes()
	if err != nil {
		return nil, err
	}

	matches := make([]*Node, 0)
	defaults := make([]*Node, 0)

	// try to find a specific match
	for _, node := range nodes {
		if strings.Compare(node.Key, "*") == 0 {
			// * key, a default match, only after others don't match
			defaults = append(defaults, node)
			continue
		}

		// a specific match
		matched, err := regexp.MatchString(node.Key, op.Service)
		if err != nil {
			// regex error
			return nil, err
		}
		if matched {
			// there may be multiple matches, so we keep looking
			matches = append(matches, node)
		}
	}

	if len(matches) > 0 {
		// pick a random node
		return matches[rand.Intn(len(matches))], nil
	}

	// if no matches, see if we have a default match
	if len(defaults) > 0 {
		return defaults[rand.Intn(len(defaults))], nil
	}

	// otherwise let's hope the service is defined by the op name
	// try the default service address, converting $service-$app-$opName into $service-$app
	opName := op.Resonator.Exec
	pos := strings.LastIndex(opName, "-")
	if pos == -1 {
		return nil, nil
	}

	opName = opName[0:pos] // we strip the step operation name and call the service

	return &Node{
		Address: opName,
		Port:    DefaultPort, // could _svc lookup, but not needed
	}, nil
}
