package autospotting

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	ec2instancesinfo "github.com/cristim/ec2-instances-info"
)

var logger, debug *log.Logger

// AutoSpotting hosts global configuration and has as methods all the public
// entrypoints of this library
type AutoSpotting struct {
	config        *Config
	hourlySavings float64
	savingsMutex  *sync.RWMutex
	mainEC2Conn   ec2iface.EC2API
}

var as *AutoSpotting

// Init initializes some data structures reusable across multiple event runs
func (a *AutoSpotting) Init(cfg *Config) {
	data, err := ec2instancesinfo.Data()
	if err != nil {
		log.Fatal(err.Error())
	}

	cfg.InstanceData = data
	a.config = cfg
	a.savingsMutex = &sync.RWMutex{}
	a.config.setupLogging()
	// use this only to list all the other regions
	a.mainEC2Conn = connectEC2(a.config.MainRegion)
	as = a
}

// ProcessCronEvent starts processing all AWS regions looking for AutoScaling groups
// enabled and taking action by replacing more pricy on-demand instances with
// compatible and cheaper spot instances.
func (a *AutoSpotting) ProcessCronEvent() {

	a.config.addDefaultFilteringMode()
	a.config.addDefaultFilter()

	allRegions, err := a.getRegions()

	if err != nil {
		logger.Println(err.Error())
		return
	}

	a.processRegions(allRegions)

}

func (cfg *Config) addDefaultFilteringMode() {
	if cfg.TagFilteringMode != "opt-out" {
		debug.Printf("Configured filtering mode: '%s', considering it as 'opt-in'(default)\n",
			cfg.TagFilteringMode)
		cfg.TagFilteringMode = "opt-in"
	} else {
		debug.Println("Configured filtering mode: 'opt-out'")
	}
}

func (cfg *Config) addDefaultFilter() {
	if len(strings.TrimSpace(cfg.FilterByTags)) == 0 {
		switch cfg.TagFilteringMode {
		case "opt-out":
			cfg.FilterByTags = "spot-enabled=false"
		default:
			cfg.FilterByTags = "spot-enabled=true"
		}
	}
}

func (cfg *Config) disableLogging() {
	cfg.LogFile = ioutil.Discard
	cfg.setupLogging()
}

func (cfg *Config) setupLogging() {
	logger = log.New(cfg.LogFile, "", cfg.LogFlag)

	if os.Getenv("AUTOSPOTTING_DEBUG") == "true" {
		debug = log.New(cfg.LogFile, "", cfg.LogFlag)
	} else {
		debug = log.New(ioutil.Discard, "", 0)
	}

}

// processAllRegions iterates all regions in parallel, and replaces instances
// for each of the ASGs tagged with tags as specified by slice represented by cfg.FilterByTags
// by default this is all asg with the tag 'spot-enabled=true'.
func (a *AutoSpotting) processRegions(regions []string) {

	var wg sync.WaitGroup

	for _, r := range regions {

		wg.Add(1)

		r := region{name: r, conf: a.config}

		go func() {

			if r.enabled() {
				logger.Printf("Enabled to run in %s, processing region.\n", r.name)
				r.processRegion()
			} else {
				debug.Println("Not enabled to run in", r.name)
				debug.Println("List of enabled regions:", r.conf.Regions)
			}

			wg.Done()
		}()
	}
	wg.Wait()
}

func connectEC2(region string) *ec2.EC2 {

	sess, err := session.NewSession()
	if err != nil {
		panic(err)
	}

	return ec2.New(sess,
		aws.NewConfig().WithRegion(region))
}

// getRegions generates a list of AWS regions.
func (a *AutoSpotting) getRegions() ([]string, error) {
	var output []string

	logger.Println("Scanning for available AWS regions")

	resp, err := a.mainEC2Conn.DescribeRegions(&ec2.DescribeRegionsInput{})

	if err != nil {
		logger.Println(err.Error())
		return nil, err
	}

	debug.Println(resp)

	for _, r := range resp.Regions {

		if r != nil && r.RegionName != nil {
			debug.Println("Found region", *r.RegionName)
			output = append(output, *r.RegionName)
		}
	}
	return output, nil
}

//EventHandler implements the event handling logic and is the main entrypoint of
// AutoSpotting
func (a *AutoSpotting) EventHandler(event *json.RawMessage) {
	var snsEvent events.SNSEvent
	var cloudwatchEvent events.CloudWatchEvent

	if event == nil {
		logger.Println("Missing event data, running as if triggered from a cron event...")
		// Event is Autospotting Cron Scheduling
		a.ProcessCronEvent()
		return
	}

	log.Println("Received event: \n", string(*event))
	parseEvent := *event

	// Try to parse event as an Sns Message
	if err := json.Unmarshal(parseEvent, &snsEvent); err != nil {
		log.Println(err.Error())
		return
	}

	// If the event comes from Sns - extract the Cloudwatch event embedded in it
	if snsEvent.Records != nil {
		snsRecord := snsEvent.Records[0]
		parseEvent = []byte(snsRecord.SNS.Message)
	}

	// Try to parse the event as Cloudwatch Event Rule
	if err := json.Unmarshal(parseEvent, &cloudwatchEvent); err != nil {
		log.Println(err.Error())
		return
	}

	// If the event is for an Instance Spot Interruption
	if cloudwatchEvent.DetailType == "EC2 Spot Instance Interruption Warning" {
		log.Println("Triggered by", cloudwatchEvent.DetailType)
		if instanceID, err := getInstanceIDDueForTermination(cloudwatchEvent); err != nil {
			log.Println("Could't get instance ID of terminating spot instance", err.Error())
			return
		} else if instanceID != nil {
			spotTermination := newSpotTermination(cloudwatchEvent.Region)
			spotTermination.executeAction(instanceID, a.config.TerminationNotificationAction)
		}

		// If event is Instance state change
	} else if cloudwatchEvent.DetailType == "EC2 Instance State-change Notification" {
		log.Println("Triggered by", cloudwatchEvent.DetailType)
		instanceID, state, err := parseEventData(cloudwatchEvent)
		if err != nil {
			log.Println("Could't get instance ID of newly launched instance", err.Error())
			return
		} else if instanceID != nil {
			a.handleNewInstanceLaunch(cloudwatchEvent.Region, *instanceID, *state)
		}

	} else {
		// Cron Scheduling
		a.ProcessCronEvent()
	}

}

func (a *AutoSpotting) handleNewInstanceLaunch(regionName string, instanceID string, state string) error {
	r := region{name: regionName, conf: a.config, services: connections{}}

	if !r.enabled() {
		return fmt.Errorf("region %s is not enabled", regionName)
	}

	r.services.connect(regionName)
	r.setupAsgFilters()
	r.scanForEnabledAutoScalingGroups()

	logger.Println("Scanning full instance information in", r.name)
	r.determineInstanceTypeInformation(r.conf)

	if err := r.scanInstance(aws.String(instanceID)); err != nil {
		logger.Printf("%s Couldn't scan instance %s: %s", regionName,
			instanceID, err.Error())
		return err
	}

	i := r.instances.get(instanceID)
	if i == nil {
		logger.Printf("%s Instance %s is missing, skipping...",
			regionName, instanceID)
		return errors.New("instance missing")
	}
	logger.Printf("%s Found instance %s in state %s",
		i.region.name, *i.InstanceId, *i.State.Name)

	if state == "pending" && i.belongsToEnabledASG() && i.shouldBeReplacedWithSpot() {
		logger.Printf("%s instance %s is in pending state, belongs to an enabled ASG "+
			"and should be replaced with spot, attempting to launch spot replacement", i.region.name, *i.InstanceId)
		if _, err := i.launchSpotReplacement(); err != nil {
			logger.Printf("%s Couldn't launch spot replacement for %s",
				i.region.name, *i.InstanceId)
			return err
		}
	} else {
		logger.Printf("%s skipping instance %s: either not in pending state (%s), doesn't "+
			"belong to an enabled ASG or should not be replaced with spot, ",
			i.region.name, *i.InstanceId, *i.State.Name)
	}

	if state == "running" {
		logger.Printf("%s Found instance %s in running state, checking if it's a spot instance "+
			"that should be attached to any ASG", i.region.name, *i.InstanceId)
		unattached := i.isUnattachedSpotInstanceLaunchedForAnEnabledASG()
		if !unattached {
			logger.Printf("%s Found instance %s is already attached to an ASG, skipping it",
				i.region.name, *i.InstanceId)
			return nil
		}

		logger.Printf("%s Found instance %s is not yet attached to its ASG, "+
			"attempting to swap it against a running on-demand instance",
			i.region.name, *i.InstanceId)

		if _, err := i.swapWithGroupMember(); err != nil {
			logger.Printf("%s, couldn't perform spot replacement of %s ",
				i.region.name, *i.InstanceId)
			return err
		}
	}

	return nil
}
