// Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cli contains the archer subcommands.
package cli

import (
	"errors"
	"fmt"

	"github.com/aws/PRIVATE-amazon-ecs-archer/cmd/archer/template"
	"github.com/aws/PRIVATE-amazon-ecs-archer/internal/pkg/archer"
	"github.com/aws/PRIVATE-amazon-ecs-archer/internal/pkg/deploy/cloudformation"
	"github.com/aws/PRIVATE-amazon-ecs-archer/internal/pkg/manifest"
	"github.com/aws/PRIVATE-amazon-ecs-archer/internal/pkg/store"
	"github.com/aws/PRIVATE-amazon-ecs-archer/internal/pkg/store/ssm"
	"github.com/aws/PRIVATE-amazon-ecs-archer/internal/pkg/term/prompt"
	"github.com/aws/PRIVATE-amazon-ecs-archer/internal/pkg/term/spinner"
	"github.com/aws/PRIVATE-amazon-ecs-archer/internal/pkg/workspace"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/spf13/cobra"
)

const defaultEnvironmentName = "test"

// InitAppOpts holds the fields to bootstrap a new application.
type InitAppOpts struct {
	Project          string `survey:"project"` // namespace that this application belongs to.
	AppName          string `survey:"name"`    // unique identifier for the application.
	AppType          string `survey:"Type"`    // type of application you're trying to build (LoadBalanced, Backend, etc.)
	ShouldDeploy     bool   // true means we should create a test environment and deploy the application in it. Exclusive with ShouldSkipDeploy.
	ShouldSkipDeploy bool   // true means we should not create a test environment and not deploy the application in it. Exclusive with ShouldDeploy.

	projStore   archer.ProjectStore
	envStore    archer.EnvironmentStore
	envDeployer archer.EnvironmentDeployer

	ws               archer.Workspace
	existingProjects []string

	prog     progress
	prompter prompter
}

// Ask prompts the user for the value of any required fields that are not already provided.
func (opts *InitAppOpts) Ask() error {
	if opts.Project == "" {
		if err := opts.projectQuestion(); err != nil {
			return err
		}
	}

	if opts.AppName == "" {
		name, err := opts.prompter.Get(
			"What is your application's name?",
			"Collection of AWS services to achieve a business capability. Must be unique within a project.",
			validateApplicationName)

		if err != nil {
			return fmt.Errorf("failed to get application name: %w", err)
		}

		opts.AppName = name
	}
	if opts.AppType == "" {
		t, err := opts.prompter.SelectOne(
			"Which template would you like to use?",
			"Pre-defined infrastructure templates.",
			[]string{manifest.LoadBalancedWebApplication})

		if err != nil {
			return fmt.Errorf("failed to get template selection: %w", err)
		}

		opts.AppType = t
	}

	return nil
}

func (opts *InitAppOpts) projectQuestion() error {
	if len(opts.existingProjects) > 0 {
		projectName, err := opts.prompter.SelectOne(
			"Which project should we use?",
			"Choose a project to create a new application in. Applications in the same project share the same VPC, ECS Cluster and are discoverable via service discovery",
			opts.existingProjects)

		if err != nil {
			return fmt.Errorf("failed to get project selection: %w", err)
		}

		opts.Project = projectName

		return nil
	}

	projectName, err := opts.prompter.Get(
		"What is your project's name?",
		"Applications under the same project share the same VPC and ECS Cluster and are discoverable via service discovery.",
		validateProjectName)

	if err != nil {
		return fmt.Errorf("failed to get project name: %w", err)
	}

	opts.Project = projectName

	return nil
}

// Validate returns an error if a command line flag provided value is invalid
func (opts *InitAppOpts) Validate() error {
	if err := validateProjectName(opts.Project); err != nil && err != errValueEmpty {
		return fmt.Errorf("project name invalid: %v", err)
	}

	if err := validateApplicationName(opts.AppName); err != nil && err != errValueEmpty {
		return fmt.Errorf("application name invalid: %v", err)
	}

	return nil
}

// Prepare loads contextual data such as any existing projects, the current workspace, etc
func (opts *InitAppOpts) Prepare() {
	// If there's a local project, we'll use that and just skip the project question.
	// Otherwise, we'll load a list of existing projects that the customer can select from.
	if opts.Project != "" {
		return
	}
	if summary, err := opts.ws.Summary(); err == nil {
		// use the project name from the workspace
		opts.Project = summary.ProjectName
		return
	}
	// load all existing project names
	existingProjects, _ := opts.projStore.ListProjects()
	var projectNames []string
	for _, p := range existingProjects {
		projectNames = append(projectNames, p.Name)
	}
	opts.existingProjects = projectNames
}

// Execute creates a project and initializes the workspace.
func (opts *InitAppOpts) Execute() error {
	if err := validateProjectName(opts.Project); err != nil {
		return err
	}

	if err := opts.createProject(); err != nil {
		return err
	}

	if err := opts.ws.Create(opts.Project); err != nil {
		return err
	}

	if err := opts.createApp(); err != nil {
		return err
	}

	return opts.deployEnv()
}
func (opts *InitAppOpts) createApp() error {
	manifest, err := manifest.Create(opts.AppName, opts.AppType)
	if err != nil {
		return fmt.Errorf("failed to generate a manifest %w", err)
	}
	manifestBytes, err := manifest.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal the manifest file %w", err)
	}
	return opts.ws.WriteManifest(manifestBytes, opts.AppName)
}

func (opts *InitAppOpts) createProject() error {
	err := opts.projStore.CreateProject(&archer.Project{
		Name: opts.Project,
	})
	// If the project already exists, that's ok - otherwise
	// return the error.
	var projectAlreadyExistsError *store.ErrProjectAlreadyExists
	if !errors.As(err, &projectAlreadyExistsError) {
		return err
	}
	return nil
}

// deployEnv prompts the user to deploy a test environment if the project doesn't already have one.
func (opts *InitAppOpts) deployEnv() error {

	if opts.ShouldSkipDeploy {
		return nil
	}

	existingEnvs, _ := opts.envStore.ListEnvironments(opts.Project)
	if len(existingEnvs) > 0 {
		return nil
	}

	deployEnv := false

	deployEnv, err := opts.prompter.Confirm(
		"Would you like to set up a test environment?",
		"You can deploy your app into your test environment.")

	if err != nil {
		// TODO: handle error?
	}

	if !deployEnv {
		return nil
	}

	// TODO: prompt the user for an environment name with default value "test"
	// https://github.com/aws/PRIVATE-amazon-ecs-archer/issues/56
	env := &archer.Environment{
		Project:            opts.Project,
		Name:               defaultEnvironmentName,
		PublicLoadBalancer: true, // TODO: configure this value based on user input or Application type needs?
	}

	opts.prog.Start("Preparing deployment...")
	if err := opts.envDeployer.DeployEnvironment(env); err != nil {
		var existsErr *cloudformation.ErrStackAlreadyExists
		if errors.As(err, &existsErr) {
			// Do nothing if the stack already exists.
			opts.prog.Stop("Done!")
			fmt.Printf("The environment %s already exists under project %s.\n", env.Name, opts.Project)
			return nil
		}
		opts.prog.Stop("Error!")
		return err
	}
	opts.prog.Stop("Done!")
	opts.prog.Start("Deploying env...")
	if err := opts.envDeployer.WaitForEnvironmentCreation(env); err != nil {
		opts.prog.Stop("Error!")
		return err
	}
	if err := opts.envStore.CreateEnvironment(env); err != nil {
		opts.prog.Stop("Error!")
		return err
	}
	opts.prog.Stop("Done!")
	return nil
}

// BuildInitCmd builds the command for bootstrapping an application.
func BuildInitCmd() *cobra.Command {
	opts := InitAppOpts{
		prompter: prompt.New(),
		prog:     spinner.New(),
	}

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a new ECS application",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			ws, err := workspace.New()
			if err != nil {
				return err
			}
			opts.ws = ws

			ssm, err := ssm.NewStore()
			if err != nil {
				return err
			}
			opts.projStore = ssm
			opts.envStore = ssm

			sess, err := session.NewSessionWithOptions(session.Options{
				SharedConfigState: session.SharedConfigEnable,
			})
			if err != nil {
				return err
			}
			opts.envDeployer = cloudformation.New(sess)

			opts.Prepare()
			return opts.Ask()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return opts.Execute()
		},
	}
	cmd.Flags().StringVarP(&opts.Project, "project", "p", "", "Name of the project.")
	cmd.Flags().StringVarP(&opts.AppName, "app", "a", "", "Name of the application.")
	cmd.Flags().StringVarP(&opts.AppType, "app-type", "t", "", "Type of application to create.")
	cmd.Flags().BoolVar(&opts.ShouldDeploy, "deploy", false, "Deploy your application to a \"test\" environment (exclusive with --skip-deploy).")
	cmd.Flags().BoolVar(&opts.ShouldSkipDeploy, "skip-deploy", false, "Skip deploying your application (exclusive with --deploy).")
	cmd.SetUsageTemplate(template.Usage)
	cmd.Annotations = map[string]string{
		"group": "Getting Started ✨",
	}
	return cmd
}
