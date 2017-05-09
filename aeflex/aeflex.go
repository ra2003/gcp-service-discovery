// aeflex implements service discovery for GCE VMs running in App Engine Flex.
package aeflex

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/kr/pretty"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	appengine "google.golang.org/api/appengine/v1"
)

const (
	aefLabel             = "__aef_"
	aefLabelProject      = aefLabel + "project"
	aefLabelService      = aefLabel + "service"
	aefLabelVersion      = aefLabel + "version"
	aefLabelInstance     = aefLabel + "instance"
	aefLabelPublicProto  = aefLabel + "public_protocol"
	aefMaxTotalInstances = aefLabel + "max_total_instances"
	aefVmDebugEnabled    = aefLabel + "vm_debug_enabled"
)

var (
	defaultScopes = []string{appengine.CloudPlatformScope, appengine.AppengineAdminScope}
)

// AEFlexSource caches information collected from the App Engine Admin API.
type AEFlexSource struct {
	// The GCP project id.
	project string

	// client caches an http client authenticated for access to GCP APIs.
	client *http.Client

	// apis is the entry point to all AEFlex services.
	apis *appengine.APIService

	// targets collects found targets.
	targets []interface{}
}

// NewAEFlexSource returns a new discovery object with an authenticated AppEngine client.
func NewAEFlexSource(project string) (*AEFlexSource, error) {
	d := &AEFlexSource{
		project: project,
	}

	var err error
	// Create a new authenticated HTTP client.
	d.client, err = google.DefaultClient(oauth2.NoContext, defaultScopes...)
	if err != nil {
		return nil, fmt.Errorf("Error setting up AppEngine client: %s", err)
	}

	// Create a new AppEngine service instance.
	d.apis, err = appengine.New(d.client)
	if err != nil {
		return nil, fmt.Errorf("Error setting up AppEngine client: %s", err)
	}

	// Allocate space for the list of targets.
	d.targets = make([]interface{}, 0)
	return d, nil
}

// getLabels creates a target configuration for a prometheus service discovery
// file. The given service version should have a "SERVING" status, the instance
// should be in a "RUNNING" state and have at least one forwarded port.
//
// In serialized form, the label set look like:
//   {
//       "labels": {
//           "__aef_instance": "aef-etl--parser-20170418t195100-abcd",
//           "__aef_max_total_instances": "20",
//           "__aef_project": "mlab-sandbox",
//           "__aef_public_protocol": "tcp",
//           "__aef_service": "etl-parser",
//           "__aef_version": "20170418t195100",
//           "__aef_vm_debug_enabled": "true"
//       },
//       "targets": [
//           "104.196.220.184:9090"
//       ]
//   }
func (client *AEFlexSource) getLabels(service *appengine.Service, version *appengine.Version, instance *appengine.Instance) map[string]interface{} {
	labels := map[string]string{
		aefLabelProject:      client.project,
		aefLabelService:      service.Id,
		aefLabelVersion:      version.Id,
		aefLabelInstance:     instance.Id,
		aefMaxTotalInstances: fmt.Sprintf("%d", version.AutomaticScaling.MaxTotalInstances),
		aefVmDebugEnabled:    fmt.Sprintf("%t", instance.VmDebugEnabled),
	}
	if strings.HasSuffix(version.Network.ForwardedPorts[0], "/udp") {
		labels[aefLabelPublicProto] = "udp"
	} else if strings.HasSuffix(version.Network.ForwardedPorts[0], "/tcp") {
		labels[aefLabelPublicProto] = "tcp"
	} else {
		labels[aefLabelPublicProto] = "both"
	}

	// TODO(dev): collect max resource sizes: cpu, memory, disk.
	//   Resources.Cpu
	//   Resources.DiskGb
	//   Resources.MemoryGb
	//   Resources.Volumes[0].Name
	//   Resources.Volumes[0].SizeGb
	//   Resources.Volumes[0].VolumeType

	// TODO: do we need to support multiple forwarded ports? How to choose?
	// Extract target address in the form of the VM public IP and forwarded port.
	re := regexp.MustCompile("([0-9]+)(/.*)")
	port := re.ReplaceAllString(version.Network.ForwardedPorts[0], "$1")
	targets := []string{fmt.Sprintf("%s:%s", instance.VmIp, port)}

	// Construct a record for the Prometheus file service discovery format.
	// https://prometheus.io/docs/operating/configuration/#<file_sd_config>
	values := map[string]interface{}{
		"labels":  labels,
		"targets": targets,
	}
	return values
}

// Save writes the content of the the collected set of targets.
func (client *AEFlexSource) Save(filename string) error {
	// Convert to JSON.
	data, err := json.MarshalIndent(client.targets, "", "    ")
	if err != nil {
		log.Printf("Failed to Marshal JSON: %s", err)
		log.Printf("Pretty data: %s", pretty.Sprint(client.targets))
		return err
	}

	// Save targets to output file.
	err = ioutil.WriteFile(filename, data, 0644)
	if err != nil {
		log.Printf("Failed to write %s: %s", filename, err)
		return err
	}
	return nil
}

// Collect contacts the App Engine Admin API to to check every service, and
// every serving version. Collect saves every AppEngine Flexible Environments
// VMs that is in a RUNNING and SERVING state.
func (client *AEFlexSource) Collect() error {
	s := client.apis.Apps.Services.List(client.project)
	// List all services.
	err := s.Pages(nil, func(listSvc *appengine.ListServicesResponse) error {
		for _, service := range listSvc.Services {
			// List all versions of each service.
			v := client.apis.Apps.Services.Versions.List(client.project, service.Id)
			err := v.Pages(nil, func(listVer *appengine.ListVersionsResponse) error {
				// pretty.Print(service)
				return client.handleVersions(listVer, service)
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	// TODO(p2, soltesz): collect and report metrics about number of API calls.
	// TODO(p2, soltesz): consider using goroutines to speed up collection.
	return err
}

// handles each version returned by an AppEngine Versions.List.
func (client *AEFlexSource) handleVersions(listVer *appengine.ListVersionsResponse, service *appengine.Service) error {
	for _, version := range listVer.Versions {

		if version.ServingStatus != "SERVING" {
			continue
		}
		// pretty.Print(version)
		// List instances associated with each service version.
		l := client.apis.Apps.Services.Versions.Instances.List(
			client.project, service.Id, version.Id)
		err := l.Pages(nil, func(listInst *appengine.ListInstancesResponse) error {
			return client.handleInstances(listInst, service, version)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// handles each version returned by an AppEngine Versions.List.
func (client *AEFlexSource) handleInstances(listInst *appengine.ListInstancesResponse, service *appengine.Service, version *appengine.Version) error {
	for _, instance := range listInst.Instances {
		// pretty.Print(instance)
		if instance.VmStatus != "RUNNING" {
			continue
		}
		// Ignore instances without networks or forwarded ports.
		if version.Network == nil {
			continue
		}
		if len(version.Network.ForwardedPorts) == 0 {
			continue
		}
		client.targets = append(
			client.targets,
			client.getLabels(service, version, instance))
	}
	return nil
}
