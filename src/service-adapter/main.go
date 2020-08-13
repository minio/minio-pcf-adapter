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
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"time"

	"github.com/pivotal-cf/on-demand-services-sdk/bosh"
	"github.com/pivotal-cf/on-demand-services-sdk/serviceadapter"
	yaml "gopkg.in/yaml.v2"
)

// Service instance name looks like service-instance_351c705a-6210-4b5e-b853-472fc8cd7646
// We strip service-instance_ and use 351c705a-6210-4b5e-b853-472fc8cd7646.[CFDOMAIN.com]
// for configuring go-router.
const instancePrefix = "service-instance_"
const tmpDir = "/tmp/minio/"

type route struct {
	Name     string   `yaml:"name"`
	Port     int      `yaml:"port"`
	Interval string   `yaml:"registration_interval"`
	Uris     []string `yaml:uris`
}

func fromPreviousManifestParameters(params map[interface{}]interface{}) map[string]interface{} {
	newMap := make(map[string]interface{})
	for k, v := range params {
		newMap[k.(string)] = v
	}
	return newMap
}

// Adapter which implements the interfaces expected by serviceadapter.
type adapter struct{}

// GenerateManifest - generates BOSH manifest file.
// func (a adapter) GenerateManifest(serviceDeployment serviceadapter.ServiceDeployment, plan serviceadapter.Plan, requestParams serviceadapter.RequestParameters, previousManifest *bosh.BoshManifest, previousPlan *serviceadapter.Plan, secrets serviceadapter.ManifestSecrets) (generateManifest serviceadapter.GenerateManifestOutput, err error) {
func (a adapter) GenerateManifest(generateParams serviceadapter.GenerateManifestParams) (generateManifest serviceadapter.GenerateManifestOutput, err error) {
	pid := os.Getpid()
	serviceDeployment := generateParams.ServiceDeployment
	outputFile := fmt.Sprintf(tmpDir+"output-%d.yml", pid)
	manifest := generateManifest.Manifest
	f, err := os.Create(outputFile) // We store the yaml instance here just for debugging purposes.
	if err != nil {
		return generateManifest, err
	}
	defer f.Close()

	var params map[string]interface{}
	var instances int
	plan := generateParams.Plan
	requestParams := generateParams.RequestParams
	previousManifest := generateParams.PreviousManifest

	if previousManifest == nil || previousManifest.Name == "" {
		// Previous manifest is not available implies that a fresh instance is getting created.
		// Instance can't be created with out -c config option providing AccessKey/SecretKey.
		if requestParams["parameters"] == nil {
			f.WriteString(`AcessKey/SecretKey configuration not provided.\n`)
			return generateManifest, errors.New(`Acesskey/Secretkey configuration not provided.\n`)
		}

		// Fresh instance is getting created.

		// Number of instances, configured in the tile.
		instances, err = strconv.Atoi(plan.Properties["instances"].(string))
		if err != nil {
			f.WriteString(`Unable to parse "instances"`)
			return generateManifest, errors.New(fmt.Sprintf(`Unable to parse "instances": %s`, err.Error()))
		}
		params = requestParams["parameters"].(map[string]interface{})
	} else {
		// Previous manifest available implies that we might be updating the plan or config.
		if requestParams["parameters"] != nil {
			params = requestParams["parameters"].(map[string]interface{})
			if params["gateway"] != nil {
				return generateManifest, errors.New(`"gateway" can be specified only during instance creation`)
			}
		} else {
			params = fromPreviousManifestParameters(previousManifest.Properties["parameters"].(map[interface{}]interface{}))
		}
		// Number of instances will always be same as previous deployment.
		instances = previousManifest.InstanceGroups[0].Instances
	}

	plan.InstanceGroups[0].Instances = instances

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
			f.WriteString(`googlecredentials should be provided for GCS`)
			return generateManifest, errors.New(`googlecredentials should be provided for GCS`)
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
		f.WriteString(fmt.Sprintf(`"%s" deployment type is not supported`, deploymentType))
		return generateManifest, errors.New(fmt.Sprintf(`"%s" deployment type is not supported`, deploymentType))
	}

	deploymentInstanceGroupsToJobs := map[string][]string{"minio-ig": []string{minioJobType, "route_registrar", "bpm"}}

	// Construct the manifest
	manifest.Name = serviceDeployment.DeploymentName
	for _, release := range serviceDeployment.Releases {
		manifest.Releases = append(manifest.Releases, bosh.Release{release.Name, release.Version})
	}
	manifest.Stemcells = []bosh.Stemcell{{"os-stemcell", serviceDeployment.Stemcells[0].OS, serviceDeployment.Stemcells[0].Version}}
	manifest.InstanceGroups, err = serviceadapter.GenerateInstanceGroupsWithNoProperties(plan.InstanceGroups, serviceDeployment.Releases, "os-stemcell", deploymentInstanceGroupsToJobs)
	if err != nil {
		fmt.Println(err)
		return generateManifest, err
	}
	if plan.Update != nil {
		manifest.Update = &bosh.Update{}
		manifest.Update.Canaries = plan.Update.Canaries
		manifest.Update.CanaryWatchTime = plan.Update.CanaryWatchTime
		manifest.Update.MaxInFlight = plan.Update.MaxInFlight
		manifest.Update.Serial = plan.Update.Serial
		manifest.Update.UpdateWatchTime = plan.Update.UpdateWatchTime
	}

	// Construct manifest properties.
	mprops := make(map[string]interface{})
	pprops := plan.Properties

	for i, job := range manifest.InstanceGroups[0].Jobs {
		if job.Name == "route_registrar" {
			manifest.InstanceGroups[0].Jobs[i] = job.AddCrossDeploymentConsumesLink("nats", "nats", pprops["deployment"].(string))
		}
	}

	mprops["parameters"] = params

	if pprops["pcf_tile_version"] != nil {
		mprops["pcf_tile_version"] = pprops["pcf_tile_version"]
	}

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
		f.WriteString("error generating manifest " + err.Error())
		return generateManifest, err
	}
	f.Write(b)
	generateManifest.Manifest = manifest
	return generateManifest, nil
}

// CreateBinding - Not implemented
func (a adapter) CreateBinding(_ serviceadapter.CreateBindingParams) (binding serviceadapter.Binding, err error) {
	return binding, errors.New("not supported")
}

// DeleteBinding - Not implemented
func (a adapter) DeleteBinding(_ serviceadapter.DeleteBindingParams) error {
	return errors.New("not supported")
}

// DashboardUrl - returns URL that looks like https://351c705a-6210-4b5e-b853-472fc8cd7646.sys.pie-27.cfplatformeng.com
// func (a adapter) DashboardUrl(instanceID string, plan serviceadapter.Plan, manifest bosh.BoshManifest) (url serviceadapter.DashboardUrl, err error) {
func (a adapter) DashboardUrl(params serviceadapter.DashboardUrlParams) (url serviceadapter.DashboardUrl, err error) {
	return serviceadapter.DashboardUrl{"https://" + params.Manifest.Properties["domain"].(string)}, nil
}

func (a adapter) GeneratePlanSchema(plan serviceadapter.GeneratePlanSchemaParams) (schema serviceadapter.PlanSchema, err error) {
	return schema, errors.New("not supported")
}

func cleanupTmpDir() {
	if err := os.Mkdir(tmpDir, 0700); err != nil {
		if !strings.Contains(err.Error(), "exists") {
			fmt.Println(err)
			os.Exit(1)
		}
	}
	yesterday := time.Now().AddDate(0, 0, -1)
	// Remove all files older than 1 day.
	filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		if info.ModTime().Before(yesterday) {
			os.Remove(path)
		}
		return nil
	})
}

func main() {
	pid := os.Getpid()
	cleanupTmpDir()
	inputFile := fmt.Sprintf(tmpDir+"input-%d.json", pid)
	input, err := os.Create(inputFile)
	if err != nil {
		fmt.Println("Unable to open input.json", err.Error())
		os.Exit(1)
	}

	b, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		fmt.Println("Unable to read from os.Stdin", err.Error())
		os.Exit(1)
	}

	fmt.Fprint(input, string(b))
	input.Close()
	input, err = os.Open(inputFile)
	if err != nil {
		fmt.Println("Unable to open input.json", err.Error())
		os.Exit(1)
	}
	os.Stdin = input

	handler := serviceadapter.CommandLineHandler{
		ManifestGenerator:     adapter{},
		Binder:                adapter{},
		DashboardURLGenerator: adapter{},
		SchemaGenerator:       adapter{},
	}
	serviceadapter.HandleCLI(os.Args, handler)
}
