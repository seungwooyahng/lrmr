package lrmr

import (
	"github.com/therne/lrmr/coordinator"
	"github.com/therne/lrmr/node"
)

type Master struct {
	node *node.Node

	jobTracker  *node.JobTracker
	nodeManager node.Manager
}

func NewMaster(crd coordinator.Coordinator, opt *Options) (*Master, error) {
	nm, err := node.NewManager(crd, opt.NodeManager)
	if err != nil {
		return nil, err
	}
	return &Master{
		node: &node.Node{
			ID:   "master",
			Host: opt.Host,
		},
		jobTracker:  node.NewJobTracker(crd),
		nodeManager: nm,
	}, nil
}

func (m *Master) Start() {
	m.jobTracker.Start()
}

func (m *Master) Stop() {
	m.jobTracker.Close()
}