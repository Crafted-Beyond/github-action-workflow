package main

import (
	"errors"
	"fmt"
	"os"
)

func (c *controller) deploymentPlanCheck() error {
	if os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || os.Getenv("DEPLOY_ACTION") != "plan" {
		return errors.New("read-only deployment checks require a manual plan")
	}
	file, owner, err := c.backendLockOwner()
	if err != nil {
		return err
	}
	if file != nil {
		return fmt.Errorf("backend is locked by run %s; wait or reconcile before planning", owner.Run)
	}
	legacy, err := c.ref(c.lockName())
	if err != nil {
		return err
	}
	if legacy != nil {
		return errors.New("legacy publication lock exists; reconcile before planning")
	}
	return nil
}
