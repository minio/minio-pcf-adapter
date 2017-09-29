package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/pivotal-cf/on-demand-services-sdk/bosh"
	"github.com/pivotal-cf/on-demand-services-sdk/serviceadapter"
)

type adapter struct{}

type route struct {
	Name     string   `yaml:"name"`
	Port     int      `yaml:"port"`
	Interval string   `yaml:"registration_interval"`
	Uris     []string `yaml:uris`
}

func (a adapter) GenerateManifest(serviceDeployment serviceadapter.ServiceDeployment, plan serviceadapter.Plan, requestParams serviceadapter.RequestParameters, previousManifest *bosh.BoshManifest, previousPlan *serviceadapter.Plan) (manifest bosh.BoshManifest, err error) {
	manifest.Name = serviceDeployment.DeploymentName
	for _, release := range serviceDeployment.Releases {
		manifest.Releases = append(manifest.Releases, bosh.Release{release.Name, release.Version})
	}
	manifest.Stemcells = []bosh.Stemcell{{"os-stemcell", serviceDeployment.Stemcell.OS, serviceDeployment.Stemcell.Version}}
	manifest.InstanceGroups, err = serviceadapter.GenerateInstanceGroupsWithNoProperties(plan.InstanceGroups, serviceDeployment.Releases, "os-stemcell", map[string][]string{
		"standalone-ig": []string{"minio-server", "route_registrar"},
	})
	if plan.Update != nil {
		manifest.Update.Canaries = plan.Update.Canaries
		manifest.Update.CanaryWatchTime = plan.Update.CanaryWatchTime
		manifest.Update.MaxInFlight = plan.Update.MaxInFlight
		manifest.Update.Serial = plan.Update.Serial
		manifest.Update.UpdateWatchTime = plan.Update.UpdateWatchTime
	}

	manifest.Properties = plan.Properties

	val := make(map[string][]route)
	val["routes"] = []route{{
		"route", 9000, "20s",
		[]string{fmt.Sprintf("%s.%s", manifest.Name, manifest.Properties["domain"].(string))}}}

	manifest.Properties["route_registrar"] = val
	return
}

func (a adapter) CreateBinding(bindingID string, deploymentTopology bosh.BoshVMs, manifest bosh.BoshManifest, requestParams serviceadapter.RequestParameters) (binding serviceadapter.Binding, err error) {
	return binding, errors.New("not supported")
}

func (a adapter) DeleteBinding(bindingID string, deploymentTopology bosh.BoshVMs, manifest bosh.BoshManifest, requestParams serviceadapter.RequestParameters) error {
	return errors.New("not supported")
}

func (a adapter) DashboardUrl(instanceID string, plan serviceadapter.Plan, manifest bosh.BoshManifest) (serviceadapter.DashboardUrl, error) {
	return serviceadapter.DashboardUrl{fmt.Sprintf("https://%s.%s", manifest.Name, manifest.Properties["domain"].(string))}, nil
}

func main() {
	args := os.Args[:5]
	args = append(args, "{}", "{}")
	serviceadapter.HandleCommandLineInvocation(args, adapter{}, adapter{}, adapter{})
}
