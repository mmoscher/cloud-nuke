package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/gruntwork-io/gruntwork-cli/collections"

	goerrors "errors"
	"github.com/fatih/color"
	"github.com/gruntwork-io/cloud-nuke/aws"
	"github.com/gruntwork-io/cloud-nuke/gcp"
	"github.com/gruntwork-io/cloud-nuke/logging"
	"github.com/gruntwork-io/gruntwork-cli/errors"
	"github.com/gruntwork-io/gruntwork-cli/shell"
	"github.com/urfave/cli"
)

// CreateCli - Create the CLI app with all commands, flags, and usage text configured.
func CreateCli(version string) *cli.App {
	app := cli.NewApp()

	app.Name = "cloud-nuke"
	app.HelpName = app.Name
	app.Author = "Gruntwork <www.gruntwork.io>"
	app.Version = version
	app.Usage = "A CLI tool to nuke (delete) cloud resources."
	app.Commands = []cli.Command{
		{
			Name:   "aws",
			Usage:  "BEWARE: DESTRUCTIVE OPERATION! Nukes AWS resources (ASG, ELB, ELBv2, EBS, EC2, AMI, Snapshots, Elastic IP).",
			Action: errors.WithPanicHandling(awsNuke),
			Flags: []cli.Flag{
				cli.StringSliceFlag{
					Name:  "exclude-region",
					Usage: "regions to exclude",
				},
				cli.StringSliceFlag{
					Name:  "resource-type",
					Usage: "Resource types to nuke",
				},
				cli.BoolFlag{
					Name:  "list-resource-types",
					Usage: "List available resource types",
				},
				cli.StringFlag{
					Name:  "older-than",
					Usage: "Only delete resources older than this specified value. Can be any valid Go duration, such as 10m or 8h.",
					Value: "0s",
				},
				cli.BoolFlag{
					Name:  "force",
					Usage: "Skip nuke confirmation prompt. WARNING: this will automatically delete all resources without any confirmation",
				},
			},
		}, {
			Name:   "defaults-aws",
			Usage:  "Nukes unused AWS defaults (VPCs, permissive security group rules) across all regions enabled for this account.",
			Action: errors.WithPanicHandling(awsDefaults),
			Flags: []cli.Flag{
				cli.BoolFlag{
					Name:  "force",
					Usage: "Skip confirmation prompt. WARNING: this will automatically delete defaults without any confirmation",
				},
			},
		},
		{
			Name:   "gcp",
			Usage:  "Clean up GCP resources (GCE instances)",
			Action: errors.WithPanicHandling(gcpNuke),
			Flags: []cli.Flag{
				cli.StringSliceFlag{
					Name:  "exclude-region",
					Usage: "regions to exclude",
				},
				cli.StringFlag{
					Name:  "older-than",
					Usage: "Only delete resources older than this specified value. Can be any valid Go duration, such as 10m or 8h.",
					Value: "0s",
				},
				cli.BoolFlag{
					Name:  "force",
					Usage: "Skip nuke confirmation prompt. WARNING: this will automatically delete all resources without any confirmation",
				},
			},
		},
	}

	return app
}

func parseDurationParam(paramValue string) (*time.Time, error) {
	duration, err := time.ParseDuration(paramValue)
	if err != nil {
		return nil, errors.WithStackTrace(err)
	}

	// make it negative so it goes back in time
	duration = -1 * duration

	excludeAfter := time.Now().Add(duration)
	return &excludeAfter, nil
}

func promptForConfirmationBeforeNuking(force bool) (bool, error) {
	if force {
		logging.Logger.Infoln("The --force flag is set, so waiting for 10 seconds before proceeding to nuke everything in your project. If you don't want to proceed, hit CTRL+C now!!")
		for i := 10; i > 0; i-- {
			fmt.Printf("%d...", i)
			time.Sleep(1 * time.Second)
		}

		fmt.Println()
		return true, nil
	} else {
		color := color.New(color.FgHiRed, color.Bold)
		color.Println("\nTHE NEXT STEPS ARE DESTRUCTIVE AND COMPLETELY IRREVERSIBLE, PROCEED WITH CAUTION!!!")

		prompt := "\nAre you sure you want to nuke all listed resources? Enter 'nuke' to confirm: "
		shellOptions := shell.ShellOptions{Logger: logging.Logger}
		input, err := shell.PromptUserForInput(prompt, &shellOptions)

		if err != nil {
			return false, errors.WithStackTrace(err)
		}

		return strings.ToLower(input) == "nuke", nil
	}
}

func regionIsValid(ctx *gcp.GcpContext, region string) bool {
	return ctx.ContainsRegion(region)
}

func gcpNuke(c *cli.Context) error {
	// TODO accept multiple credentials and nuke resources on all the projects
	// specified by a command line parameter we have authorization for.
	ctx, err := gcp.DefaultContext()
	if err != nil {
		return errors.WithStackTrace(err)
	}

	logging.Logger.Infof("Using project: %s", ctx.Project)

	excludedRegions := c.StringSlice("exclude-region")

	for _, excludedRegion := range excludedRegions {
		if !regionIsValid(ctx, excludedRegion) {
			return InvalidFlagError{
				Name:  "exclude-region",
				Value: excludedRegion,
			}
		}
	}

	excludeAfter, err := parseDurationParam(c.String("older-than"))
	if err != nil {
		return errors.WithStackTrace(err)
	}

	logging.Logger.Infoln("Retrieving all active GCP resources")

	resources, err := ctx.GetAllResources(excludedRegions, *excludeAfter)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	if len(resources) == 0 {
		logging.Logger.Infoln("Nothing to nuke, you're all good!")
		return nil
	}

	logging.Logger.Infoln("The following GCP resources are going to be nuked: ")

	for _, resource := range resources {
		logging.Logger.Infof("* %s: %s Region=%s Zone=%s",
			resource.Kind(), resource.Name(), resource.Region(), resource.Zone())
	}

	confirmation, err := promptForConfirmationBeforeNuking(c.Bool("force"))
	if err != nil {
		return err
	}

	if confirmation {
		nukeErrors := ctx.NukeAllResources(resources)
		if len(nukeErrors) != 0 {
			for _, err := range nukeErrors {
				logging.Logger.Errorf(errors.WithStackTrace(err).Error())
			}
			return goerrors.New("Some resources failed to nuke.")
		}
	}

	return nil
}

func awsNuke(c *cli.Context) error {
	allResourceTypes := aws.ListResourceTypes()

	if c.Bool("list-resource-types") {
		for _, resourceType := range aws.ListResourceTypes() {
			fmt.Println(resourceType)
		}
		return nil
	}

	resourceTypes := c.StringSlice("resource-type")
	var invalidresourceTypes []string
	for _, resourceType := range resourceTypes {
		if resourceType == "all" {
			continue
		}
		if !aws.IsValidResourceType(resourceType, allResourceTypes) {
			invalidresourceTypes = append(invalidresourceTypes, resourceType)
		}
	}

	if len(invalidresourceTypes) > 0 {
		msg := "Try --list-resource-types to get list of valid resource types."
		return fmt.Errorf("Invalid resourceTypes %s specified: %s", invalidresourceTypes, msg)
	}

	regions, err := aws.GetEnabledRegions()
	if err != nil {
		return errors.WithStackTrace(err)
	}
	excludedRegions := c.StringSlice("exclude-region")

	for _, excludedRegion := range excludedRegions {
		if !collections.ListContainsElement(regions, excludedRegion) {
			return InvalidFlagError{
				Name:  "exclude-regions",
				Value: excludedRegion,
			}
		}
	}

	excludeAfter, err := parseDurationParam(c.String("older-than"))
	if err != nil {
		return errors.WithStackTrace(err)
	}

	logging.Logger.Infoln("Retrieving all active AWS resources")
	account, err := aws.GetAllResources(regions, excludedRegions, *excludeAfter, resourceTypes)

	if err != nil {
		return errors.WithStackTrace(err)
	}

	if len(account.Resources) == 0 {
		logging.Logger.Infoln("Nothing to nuke, you're all good!")
		return nil
	}

	logging.Logger.Infoln("The following AWS resources are going to be nuked: ")

	for region, resourcesInRegion := range account.Resources {
		for _, resources := range resourcesInRegion.Resources {
			for _, identifier := range resources.ResourceIdentifiers() {
				logging.Logger.Infof("* %s-%s-%s\n", resources.ResourceName(), identifier, region)
			}
		}
	}

	if !c.Bool("force") {
		prompt := "\nAre you sure you want to nuke all listed resources? Enter 'nuke' to confirm: "
		proceed, err := confirmationPrompt(prompt)
		if err != nil {
			return err
		}
		if proceed {
			if err := aws.NukeAllResources(account, regions); err != nil {
				return err
			}
		}
	} else {
		logging.Logger.Infoln("The --force flag is set, so waiting for 10 seconds before proceeding to nuke everything in your account. If you don't want to proceed, hit CTRL+C now!!")
		for i := 10; i > 0; i-- {
			fmt.Printf("%d...", i)
			time.Sleep(1 * time.Second)
		}

		fmt.Println()
		if err := aws.NukeAllResources(account, regions); err != nil {
			return err
		}
	}

	return nil
}

func awsDefaults(c *cli.Context) error {
	logging.Logger.Infoln("Identifying enabled regions")
	regions, err := aws.GetEnabledRegions()
	if err != nil {
		return errors.WithStackTrace(err)
	}
	for _, region := range regions {
		logging.Logger.Infof("Found enabled region %s", region)
	}

	err = nukeDefaultVpcs(c, regions)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	err = nukeDefaultSecurityGroups(c, regions)
	if err != nil {
		return errors.WithStackTrace(err)
	}
	return nil
}

func nukeDefaultVpcs(c *cli.Context, regions []string) error {
	logging.Logger.Infof("Discovering default VPCs")
	vpcPerRegion := aws.NewVpcPerRegion(regions)
	vpcPerRegion, err := aws.GetDefaultVpcs(vpcPerRegion)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	if len(vpcPerRegion) == 0 {
		logging.Logger.Info("No default VPCs found.")
		return nil
	}

	for _, vpc := range vpcPerRegion {
		logging.Logger.Infof("* Default VPC %s %s", vpc.VpcId, vpc.Region)
	}

	var proceed bool
	if !c.Bool("force") {
		prompt := "\nAre you sure you want to nuke all default VPCs? Enter 'nuke' to confirm: "
		proceed, err = confirmationPrompt(prompt)
		if err != nil {
			return err
		}
	}

	if proceed || c.Bool("force") {
		err := aws.NukeVpcs(vpcPerRegion)
		if err != nil {
			logging.Logger.Errorf("[Failed] %s", err)
		}
	}
	return nil
}

func nukeDefaultSecurityGroups(c *cli.Context, regions []string) error {
	logging.Logger.Infof("Discovering default security groups")
	defaultSgs, err := aws.GetDefaultSecurityGroups(regions)
	if err != nil {
		return errors.WithStackTrace(err)
	}

	for _, sg := range defaultSgs {
		logging.Logger.Infof("* Default rules for SG %s %s %s", sg.GroupId, sg.GroupName, sg.Region)
	}

	var proceed bool
	if !c.Bool("force") {
		prompt := "\nAre you sure you want to nuke the rules in these default security groups ? Enter 'nuke' to confirm: "
		proceed, err = confirmationPrompt(prompt)
		if err != nil {
			return err
		}
	}

	if proceed || c.Bool("force") {
		err := aws.NukeDefaultSecurityGroupRules(defaultSgs)
		if err != nil {
			logging.Logger.Errorf("[Failed] %s", err)
		}
	}
	return nil
}

func confirmationPrompt(prompt string) (bool, error) {
	color := color.New(color.FgHiRed, color.Bold)
	color.Println("\nTHE NEXT STEPS ARE DESTRUCTIVE AND COMPLETELY IRREVERSIBLE, PROCEED WITH CAUTION!!!")

	shellOptions := shell.ShellOptions{Logger: logging.Logger}
	input, err := shell.PromptUserForInput(prompt, &shellOptions)

	if err != nil {
		return false, errors.WithStackTrace(err)
	}

	if strings.ToLower(input) == "nuke" {
		return true, nil
	}

	return false, nil
}
