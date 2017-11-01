/*
 * Minio Cloud Storage, (C) 2016,2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pivotal-cf/on-demand-services-sdk/bosh"
	"github.com/pivotal-cf/on-demand-services-sdk/serviceadapter"
	"gopkg.in/yaml.v2"
)

// Service instance name looks like service-instance_351c705a-6210-4b5e-b853-472fc8cd7646
// We strip service-instance_ and use 351c705a-6210-4b5e-b853-472fc8cd7646.[CFDOMAIN.com]
// for configuring go-router.
const instancePrefix = "service-instance_"

type route struct {
	Name     string   `yaml:"name"`
	Port     int      `yaml:"port"`
	Interval string   `yaml:"registration_interval"`
	Uris     []string `yaml:uris`
}

// Adapter which implements the interfaces expected by serviceadapter.
type adapter struct{}

// GenerateManifest - generates BOSH manifest file.
func (a adapter) GenerateManifest(serviceDeployment serviceadapter.ServiceDeployment, plan serviceadapter.Plan, requestParams serviceadapter.RequestParameters, previousManifest *bosh.BoshManifest, previousPlan *serviceadapter.Plan) (manifest bosh.BoshManifest, err error) {
	f, err := os.Create("/tmp/adapter.log") // We store the yaml instance here just for debugging purposes.
	if err != nil {
		return manifest, err
	}
	defer f.Close()

	if requestParams["parameters"] == nil {
		return manifest, errors.New(`configuration not provided, please use "-c" option to provide configuration`)
	}

	// Number of instances, configured in the tile.
	instances, err := strconv.Atoi(plan.Properties["instances"].(string))
	if err != nil {
		return manifest, errors.New(fmt.Sprintf(`Unable to parse "instances": %s`, err.Error()))
	}
	plan.InstanceGroups[0].Instances = instances

	params := requestParams["parameters"].(map[string]interface{})

	// If the number of instances is configured as 1 then we allow fs, gcs, azure.
	// If the number of instances is not 1 then we allow only erasure.
	deploymentType := "fs"
	if plan.InstanceGroups[0].Instances != 1 {
		deploymentType = "erasure"
	}

	if params["gateway"] != nil {
		deploymentType = params["gateway"].(string)
	}

	if deploymentType == "gcs" {
		if params["googlecredentials"] == nil {
			return manifest, errors.New(`googlecredentials should be provided for GCS`)
		}
	}
	var minioJobType string
	switch deploymentType {
	case "fs", "erasure":
		minioJobType = "minio-server"
	case "azure":
		minioJobType = "minio-azure"
	case "gcs":
		minioJobType = "minio-gcs"
	default:
		return manifest, errors.New(fmt.Sprintf(`"%s" deployment type is not supported`, deploymentType))
	}

	deploymentInstanceGroupsToJobs := map[string][]string{"minio-ig": []string{minioJobType, "route_registrar"}}

	// Construct the manifest
	manifest.Name = serviceDeployment.DeploymentName
	for _, release := range serviceDeployment.Releases {
		manifest.Releases = append(manifest.Releases, bosh.Release{release.Name, release.Version})
	}
	manifest.Stemcells = []bosh.Stemcell{{"os-stemcell", serviceDeployment.Stemcell.OS, serviceDeployment.Stemcell.Version}}
	manifest.InstanceGroups, err = serviceadapter.GenerateInstanceGroupsWithNoProperties(plan.InstanceGroups, serviceDeployment.Releases, "os-stemcell", deploymentInstanceGroupsToJobs)
	if plan.Update != nil {
		manifest.Update.Canaries = plan.Update.Canaries
		manifest.Update.CanaryWatchTime = plan.Update.CanaryWatchTime
		manifest.Update.MaxInFlight = plan.Update.MaxInFlight
		manifest.Update.Serial = plan.Update.Serial
		manifest.Update.UpdateWatchTime = plan.Update.UpdateWatchTime
	}

	// Construct manifest properties.
	mprops := make(map[string]interface{})
	pprops := plan.Properties

	if pprops["pcf_tile_version"] != nil {
		mprops["pcf_tile_version"] = pprops["pcf_tile_version"]
	}
	mprops["nats"] = pprops["nats"]

	domain := fmt.Sprintf("%s.%s", strings.TrimPrefix(manifest.Name, instancePrefix), pprops["domain"].(string))
	subdomain := params["subdomain"]
	if subdomain != nil {
		// If cf create-service passed subdomain value, then use it.
		domain = fmt.Sprintf("%s.storage.%s", subdomain.(string), pprops["domain"].(string))
	}
	mprops["domain"] = domain
	mprops["route_registrar"] = map[string][]route{
		"routes": []route{
			{
				"route", 9000, "20s",
				[]string{domain},
			},
		},
	}
	credential := make(map[string]string)
	credential["accesskey"] = params["accesskey"].(string)
	credential["secretkey"] = params["secretkey"].(string)
	if params["googlecredentials"] != nil {
		credential["googlecredentials"] = params["googlecredentials"].(string)
	}
	mprops["credential"] = credential
	manifest.Properties = mprops
	b, err := yaml.Marshal(manifest)
	if err != nil {
		return manifest, err
	}
	f.Write(b)
	return manifest, nil
}

// CreateBinding - Not implemented
func (a adapter) CreateBinding(bindingID string, deploymentTopology bosh.BoshVMs, manifest bosh.BoshManifest, requestParams serviceadapter.RequestParameters) (binding serviceadapter.Binding, err error) {
	return binding, errors.New("not supported")
}

// DeleteBinding - Not implemented
func (a adapter) DeleteBinding(bindingID string, deploymentTopology bosh.BoshVMs, manifest bosh.BoshManifest, requestParams serviceadapter.RequestParameters) error {
	return errors.New("not supported")
}

// DashboardUrl - returns URL that looks like https://351c705a-6210-4b5e-b853-472fc8cd7646.sys.pie-27.cfplatformeng.com
func (a adapter) DashboardUrl(instanceID string, plan serviceadapter.Plan, manifest bosh.BoshManifest) (url serviceadapter.DashboardUrl, err error) {
	return serviceadapter.DashboardUrl{"https://" + manifest.Properties["domain"].(string)}, nil
}

func main() {
	// service-adapter generate-manifest <service-deployment-JSON> <plan-JSON> <request-params-JSON> <previous-manifest-YAML> <previous-plan-JSON>
	// ODB calls us with empty strings for <previous-manifest-YAML> <previous-plan-JSON>
	// because of which json.Unmarshal fails, hence we pass {} in place of empty strings.
	args := os.Args
	if len(os.Args) > 5 {
		args = os.Args[:5]
	}
	args = append(args, "{}", "{}")
	serviceadapter.HandleCommandLineInvocation(args, adapter{}, adapter{}, adapter{})
}
