package apply

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	kyvernov2 "github.com/kyverno/kyverno/api/kyverno/v2"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/command"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/deprecations"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/exception"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/log"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/output/color"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/policy"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/processor"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/source"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/store"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/userinfo"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/utils/common"
	"github.com/kyverno/kyverno/cmd/cli/kubectl-kyverno/variables"
	"github.com/kyverno/kyverno/pkg/autogen"
	"github.com/kyverno/kyverno/pkg/clients/dclient"
	"github.com/kyverno/kyverno/pkg/config"
	engineapi "github.com/kyverno/kyverno/pkg/engine/api"
	gitutils "github.com/kyverno/kyverno/pkg/utils/git"
	policyvalidation "github.com/kyverno/kyverno/pkg/validation/policy"
	"github.com/spf13/cobra"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const divider = "----------------------------------------------------------------------"

type SkippedInvalidPolicies struct {
	skipped []string
	invalid []string
}

type ApplyCommandConfig struct {
	KubeConfig            string
	Context               string
	Namespace             string
	MutateLogPath         string
	Variables             []string
	ValuesFile            string
	UserInfoPath          string
	Cluster               bool
	PolicyReport          bool
	Stdin                 bool
	RegistryAccess        bool
	AuditWarn             bool
	ResourcePaths         []string
	PolicyPaths           []string
	GitBranch             string
	warnExitCode          int
	warnNoPassed          bool
	Exception             []string
	ContinueOnFail        bool
	inlineExceptions      bool
	GenerateExceptions    bool
	GeneratedExceptionTTL time.Duration
}

func Command() *cobra.Command {
	var removeColor, detailedResults, table bool
	applyCommandConfig := &ApplyCommandConfig{}
	cmd := &cobra.Command{
		Use:          "apply",
		Short:        command.FormatDescription(true, websiteUrl, false, description...),
		Long:         command.FormatDescription(false, websiteUrl, false, description...),
		Example:      command.FormatExamples(examples...),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			out := cmd.OutOrStdout()
			color.Init(removeColor)
			applyCommandConfig.PolicyPaths = args
			rc, _, skipInvalidPolicies, responses, err := applyCommandConfig.applyCommandHelper(out)
			if err != nil {
				return err
			}
			cmd.SilenceErrors = true
			printSkippedAndInvalidPolicies(out, skipInvalidPolicies)
			if applyCommandConfig.PolicyReport {
				printReports(out, responses, applyCommandConfig.AuditWarn)
			} else if applyCommandConfig.GenerateExceptions {
				printExceptions(out, responses, applyCommandConfig.AuditWarn, applyCommandConfig.GeneratedExceptionTTL)
			} else if table {
				printTable(out, detailedResults, applyCommandConfig.AuditWarn, responses...)
			} else {
				for _, response := range responses {
					var failedRules []engineapi.RuleResponse
					resPath := fmt.Sprintf("%s/%s/%s", response.Resource.GetNamespace(), response.Resource.GetKind(), response.Resource.GetName())
					for _, rule := range response.PolicyResponse.Rules {
						if rule.Status() == engineapi.RuleStatusFail {
							failedRules = append(failedRules, rule)
						}
						if rule.RuleType() == engineapi.Mutation {
							if rule.Status() == engineapi.RuleStatusSkip {
								fmt.Fprintln(out, "\nskipped mutate policy", response.Policy().GetName(), "->", "resource", resPath)
							} else if rule.Status() == engineapi.RuleStatusError {
								fmt.Fprintln(out, "\nerror while applying mutate policy", response.Policy().GetName(), "->", "resource", resPath, "\nerror: ", rule.Message())
							}
						}
					}
					if len(failedRules) > 0 {
						auditWarn := false
						if applyCommandConfig.AuditWarn && response.GetValidationFailureAction().Audit() {
							auditWarn = true
						}
						if auditWarn {
							fmt.Fprintln(out, "policy", response.Policy().GetName(), "->", "resource", resPath, "failed as audit warning:")
						} else {
							fmt.Fprintln(out, "policy", response.Policy().GetName(), "->", "resource", resPath, "failed:")
						}
						for i, rule := range failedRules {
							fmt.Fprintln(out, i+1, "-", rule.Name(), rule.Message())
						}
						fmt.Fprintln(out, "")
					}
				}
				printViolations(out, rc)
			}
			return exit(out, rc, applyCommandConfig.warnExitCode, applyCommandConfig.warnNoPassed)
		},
	}
	cmd.Flags().StringSliceVarP(&applyCommandConfig.ResourcePaths, "resource", "r", []string{}, "Path to resource files")
	cmd.Flags().StringSliceVarP(&applyCommandConfig.ResourcePaths, "resources", "", []string{}, "Path to resource files")
	cmd.Flags().BoolVarP(&applyCommandConfig.Cluster, "cluster", "c", false, "Checks if policies should be applied to cluster in the current context")
	cmd.Flags().StringVarP(&applyCommandConfig.MutateLogPath, "output", "o", "", "Prints the mutated/generated resources in provided file/directory")
	// currently `set` flag supports variable for single policy applied on single resource
	cmd.Flags().StringVarP(&applyCommandConfig.UserInfoPath, "userinfo", "u", "", "Admission Info including Roles, Cluster Roles and Subjects")
	cmd.Flags().StringSliceVarP(&applyCommandConfig.Variables, "set", "s", nil, "Variables that are required")
	cmd.Flags().StringVarP(&applyCommandConfig.ValuesFile, "values-file", "f", "", "File containing values for policy variables")
	cmd.Flags().BoolVarP(&applyCommandConfig.PolicyReport, "policy-report", "p", false, "Generates policy report when passed (default policyviolation)")
	cmd.Flags().StringVarP(&applyCommandConfig.Namespace, "namespace", "n", "", "Optional Policy parameter passed with cluster flag")
	cmd.Flags().BoolVarP(&applyCommandConfig.Stdin, "stdin", "i", false, "Optional mutate policy parameter to pipe directly through to kubectl")
	cmd.Flags().BoolVar(&applyCommandConfig.RegistryAccess, "registry", false, "If set to true, access the image registry using local docker credentials to populate external data")
	cmd.Flags().StringVar(&applyCommandConfig.KubeConfig, "kubeconfig", "", "path to kubeconfig file with authorization and master location information")
	cmd.Flags().StringVar(&applyCommandConfig.Context, "context", "", "The name of the kubeconfig context to use")
	cmd.Flags().StringVarP(&applyCommandConfig.GitBranch, "git-branch", "b", "", "test git repository branch")
	cmd.Flags().BoolVar(&applyCommandConfig.AuditWarn, "audit-warn", false, "If set to true, will flag audit policies as warnings instead of failures")
	cmd.Flags().IntVar(&applyCommandConfig.warnExitCode, "warn-exit-code", 0, "Set the exit code for warnings; if failures or errors are found, will exit 1")
	cmd.Flags().BoolVar(&applyCommandConfig.warnNoPassed, "warn-no-pass", false, "Specify if warning exit code should be raised if no objects satisfied a policy; can be used together with --warn-exit-code flag")
	cmd.Flags().BoolVar(&removeColor, "remove-color", false, "Remove any color from output")
	cmd.Flags().BoolVar(&detailedResults, "detailed-results", false, "If set to true, display detailed results")
	cmd.Flags().BoolVarP(&table, "table", "t", false, "Show results in table format")
	cmd.Flags().StringSliceVarP(&applyCommandConfig.Exception, "exception", "e", nil, "Policy exception to be considered when evaluating policies against resources")
	cmd.Flags().StringSliceVarP(&applyCommandConfig.Exception, "exceptions", "", nil, "Policy exception to be considered when evaluating policies against resources")
	cmd.Flags().BoolVar(&applyCommandConfig.ContinueOnFail, "continue-on-fail", false, "If set to true, will continue to apply policies on the next resource upon failure to apply to the current resource instead of exiting out")
	cmd.Flags().BoolVarP(&applyCommandConfig.inlineExceptions, "exceptions-with-resources", "", false, "Evaluate policy exceptions from the resources path")
	cmd.Flags().BoolVarP(&applyCommandConfig.GenerateExceptions, "generate-exceptions", "", false, "Generate policy exceptions for each violation")
	cmd.Flags().DurationVarP(&applyCommandConfig.GeneratedExceptionTTL, "generated-exception-ttl", "", time.Hour*24*30, "Default TTL for generated exceptions")
	return cmd
}

func (c *ApplyCommandConfig) applyCommandHelper(out io.Writer) (*processor.ResultCounts, []*unstructured.Unstructured, SkippedInvalidPolicies, []engineapi.EngineResponse, error) {
	rc, resources1, skipInvalidPolicies, responses1, err := c.checkArguments()
	if err != nil {
		return rc, resources1, skipInvalidPolicies, responses1, err
	}
	rc, resources1, skipInvalidPolicies, responses1, err, mutateLogPathIsDir := c.getMutateLogPathIsDir(skipInvalidPolicies)
	if err != nil {
		return rc, resources1, skipInvalidPolicies, responses1, err
	}
	rc, resources1, skipInvalidPolicies, responses1, err = c.cleanPreviousContent(mutateLogPathIsDir, skipInvalidPolicies)
	if err != nil {
		return rc, resources1, skipInvalidPolicies, responses1, err
	}
	var userInfo *kyvernov2.RequestInfo
	if c.UserInfoPath != "" {
		info, err := userinfo.Load(nil, c.UserInfoPath, "")
		if err != nil {
			return nil, nil, skipInvalidPolicies, nil, fmt.Errorf("failed to load request info (%w)", err)
		}
		deprecations.CheckUserInfo(out, c.UserInfoPath, info)
		userInfo = &info.RequestInfo
	}
	variables, err := variables.New(out, nil, "", c.ValuesFile, nil, c.Variables...)
	if err != nil {
		return nil, nil, skipInvalidPolicies, nil, fmt.Errorf("failed to decode yaml (%w)", err)
	}
	var store store.Store
	rc, resources1, skipInvalidPolicies, responses1, dClient, err := c.initStoreAndClusterClient(&store, skipInvalidPolicies)
	if err != nil {
		return rc, resources1, skipInvalidPolicies, responses1, err
	}
	rc, resources1, skipInvalidPolicies, responses1, policies, vaps, vapBindings, err := c.loadPolicies(skipInvalidPolicies)
	if err != nil {
		return rc, resources1, skipInvalidPolicies, responses1, err
	}
	resources, err := c.loadResources(out, policies, vaps, dClient)
	if err != nil {
		return rc, resources1, skipInvalidPolicies, responses1, err
	}
	var exceptions []*kyvernov2.PolicyException
	if c.inlineExceptions {
		exceptions = exception.SelectFrom(resources)
	} else {
		exceptions, err = exception.Load(c.Exception...)
		if err != nil {
			return rc, resources1, skipInvalidPolicies, responses1, fmt.Errorf("Error: failed to load exceptions (%s)", err)
		}
	}
	if !c.Stdin && !c.PolicyReport && !c.GenerateExceptions {
		var policyRulesCount int
		for _, policy := range policies {
			policyRulesCount += len(autogen.ComputeRules(policy, ""))
		}
		policyRulesCount += len(vaps)
		if len(exceptions) > 0 {
			fmt.Fprintf(out, "\nApplying %d policy rule(s) to %d resource(s) with %d exception(s)...\n", policyRulesCount, len(resources), len(exceptions))
		} else {
			fmt.Fprintf(out, "\nApplying %d policy rule(s) to %d resource(s)...\n", policyRulesCount, len(resources))
		}
	}

	rc, resources1, responses1, err = c.applyPolicytoResource(
		out,
		&store,
		variables,
		policies,
		resources,
		exceptions,
		&skipInvalidPolicies,
		dClient,
		userInfo,
		mutateLogPathIsDir,
	)
	if err != nil {
		return rc, resources1, skipInvalidPolicies, responses1, err
	}
	responses2, err := c.applyValidatingAdmissionPolicytoResource(vaps, vapBindings, resources1, variables.NamespaceSelectors(), rc, dClient)
	if err != nil {
		return rc, resources1, skipInvalidPolicies, responses1, err
	}
	var responses []engineapi.EngineResponse
	responses = append(responses, responses1...)
	responses = append(responses, responses2...)
	return rc, resources1, skipInvalidPolicies, responses, nil
}

func (c *ApplyCommandConfig) getMutateLogPathIsDir(skipInvalidPolicies SkippedInvalidPolicies) (*processor.ResultCounts, []*unstructured.Unstructured, SkippedInvalidPolicies, []engineapi.EngineResponse, error, bool) {
	mutateLogPathIsDir, err := checkMutateLogPath(c.MutateLogPath)
	if err != nil {
		return nil, nil, skipInvalidPolicies, nil, fmt.Errorf("failed to create file/folder (%w)", err), false
	}
	return nil, nil, skipInvalidPolicies, nil, err, mutateLogPathIsDir
}

func (c *ApplyCommandConfig) applyValidatingAdmissionPolicytoResource(
	vaps []admissionregistrationv1beta1.ValidatingAdmissionPolicy,
	vapBindings []admissionregistrationv1beta1.ValidatingAdmissionPolicyBinding,
	resources []*unstructured.Unstructured,
	namespaceSelectorMap map[string]map[string]string,
	rc *processor.ResultCounts,
	dClient dclient.Interface,
) ([]engineapi.EngineResponse, error) {
	var responses []engineapi.EngineResponse
	for _, resource := range resources {
		processor := processor.ValidatingAdmissionPolicyProcessor{
			Policies:             vaps,
			Bindings:             vapBindings,
			Resource:             resource,
			NamespaceSelectorMap: namespaceSelectorMap,
			PolicyReport:         c.PolicyReport,
			Rc:                   rc,
			Client:               dClient,
		}
		ers, err := processor.ApplyPolicyOnResource()
		if err != nil {
			if c.ContinueOnFail {
				fmt.Printf("failed to apply policies on resource %s (%v)\n", resource.GetName(), err)
				continue
			}
			return responses, fmt.Errorf("failed to apply policies on resource %s (%w)", resource.GetName(), err)
		}
		responses = append(responses, ers...)
	}
	return responses, nil
}

func (c *ApplyCommandConfig) applyPolicytoResource(
	out io.Writer,
	store *store.Store,
	vars *variables.Variables,
	policies []kyvernov1.PolicyInterface,
	resources []*unstructured.Unstructured,
	exceptions []*kyvernov2.PolicyException,
	skipInvalidPolicies *SkippedInvalidPolicies,
	dClient dclient.Interface,
	userInfo *kyvernov2.RequestInfo,
	mutateLogPathIsDir bool,
) (*processor.ResultCounts, []*unstructured.Unstructured, []engineapi.EngineResponse, error) {
	if vars != nil {
		vars.SetInStore(store)
	}
	var rc processor.ResultCounts
	// validate policies
	validPolicies := make([]kyvernov1.PolicyInterface, 0, len(policies))
	for _, pol := range policies {
		// TODO we should return this info to the caller
		sa := config.KyvernoUserName(config.KyvernoServiceAccountName())
		_, err := policyvalidation.Validate(pol, nil, nil, nil, true, sa, sa)
		if err != nil {
			log.Log.Error(err, "policy validation error")
			rc.IncrementError(1)
			if strings.HasPrefix(err.Error(), "variable 'element.name'") {
				skipInvalidPolicies.invalid = append(skipInvalidPolicies.invalid, pol.GetName())
			} else {
				skipInvalidPolicies.skipped = append(skipInvalidPolicies.skipped, pol.GetName())
			}
			continue
		}
		validPolicies = append(validPolicies, pol)
	}

	var responses []engineapi.EngineResponse
	for _, resource := range resources {
		processor := processor.PolicyProcessor{
			Store:                store,
			Policies:             validPolicies,
			Resource:             *resource,
			PolicyExceptions:     exceptions,
			MutateLogPath:        c.MutateLogPath,
			MutateLogPathIsDir:   mutateLogPathIsDir,
			Variables:            vars,
			UserInfo:             userInfo,
			PolicyReport:         c.PolicyReport,
			NamespaceSelectorMap: vars.NamespaceSelectors(),
			Stdin:                c.Stdin,
			Rc:                   &rc,
			PrintPatchResource:   true,
			Client:               dClient,
			AuditWarn:            c.AuditWarn,
			Subresources:         vars.Subresources(),
			Out:                  out,
		}
		ers, err := processor.ApplyPoliciesOnResource()
		if err != nil {
			if c.ContinueOnFail {
				log.Log.Info(fmt.Sprintf("failed to apply policies on resource %s (%s)\n", resource.GetName(), err.Error()))
				continue
			}
			return &rc, resources, responses, fmt.Errorf("failed to apply policies on resource %s (%w)", resource.GetName(), err)
		}
		responses = append(responses, ers...)
	}
	for _, policy := range validPolicies {
		if policy.GetNamespace() == "" && policy.GetKind() == "Policy" {
			log.Log.Info(fmt.Sprintf("Policy %s has no namespace detected. Ensure that namespaced policies are correctly loaded.", policy.GetNamespace()))
		}
	}
	return &rc, resources, responses, nil
}

func (c *ApplyCommandConfig) loadResources(out io.Writer, policies []kyvernov1.PolicyInterface, vap []admissionregistrationv1beta1.ValidatingAdmissionPolicy, dClient dclient.Interface) ([]*unstructured.Unstructured, error) {
	resources, err := common.GetResourceAccordingToResourcePath(out, nil, c.ResourcePaths, c.Cluster, policies, vap, dClient, c.Namespace, c.PolicyReport, "")
	if err != nil {
		return resources, fmt.Errorf("failed to load resources (%w)", err)
	}
	return resources, nil
}

func (c *ApplyCommandConfig) loadPolicies(skipInvalidPolicies SkippedInvalidPolicies) (*processor.ResultCounts, []*unstructured.Unstructured, SkippedInvalidPolicies, []engineapi.EngineResponse, []kyvernov1.PolicyInterface, []admissionregistrationv1beta1.ValidatingAdmissionPolicy, []admissionregistrationv1beta1.ValidatingAdmissionPolicyBinding, error) {
	// load policies
	var policies []kyvernov1.PolicyInterface
	var vaps []admissionregistrationv1beta1.ValidatingAdmissionPolicy
	var vapBindings []admissionregistrationv1beta1.ValidatingAdmissionPolicyBinding

	for _, path := range c.PolicyPaths {
		isGit := source.IsGit(path)
		if isGit {
			gitSourceURL, err := url.Parse(path)
			if err != nil {
				return nil, nil, skipInvalidPolicies, nil, nil, nil, nil, fmt.Errorf("failed to load policies (%w)", err)
			}
			pathElems := strings.Split(gitSourceURL.Path[1:], "/")
			if len(pathElems) <= 1 {
				err := fmt.Errorf("invalid URL path %s - expected https://<any_git_source_domain>/:owner/:repository/:branch (without --git-branch flag) OR https://<any_git_source_domain>/:owner/:repository/:directory (with --git-branch flag)", gitSourceURL.Path)
				return nil, nil, skipInvalidPolicies, nil, nil, nil, nil, fmt.Errorf("failed to parse URL (%w)", err)
			}
			gitSourceURL.Path = strings.Join([]string{pathElems[0], pathElems[1]}, "/")
			repoURL := gitSourceURL.String()
			var gitPathToYamls string
			c.GitBranch, gitPathToYamls = common.GetGitBranchOrPolicyPaths(c.GitBranch, repoURL, path)
			fs := memfs.New()
			if _, err := gitutils.Clone(repoURL, fs, c.GitBranch); err != nil {
				log.Log.V(3).Info(fmt.Sprintf("failed to clone repository  %v as it is not valid", repoURL), "error", err)
				return nil, nil, skipInvalidPolicies, nil, nil, nil, nil, fmt.Errorf("failed to clone repository (%w)", err)
			}
			policyYamls, err := gitutils.ListYamls(fs, gitPathToYamls)
			if err != nil {
				return nil, nil, skipInvalidPolicies, nil, nil, nil, nil, fmt.Errorf("failed to list YAMLs in repository (%w)", err)
			}
			for _, policyYaml := range policyYamls {
				loaderResults, err := policy.Load(fs, "", policyYaml)
				if loaderResults != nil && loaderResults.NonFatalErrors != nil {
					for _, err := range loaderResults.NonFatalErrors {
						log.Log.Error(err.Error, "Non-fatal parsing error for single document")
					}
				}
				if err != nil {
					continue
				}
				policies = append(policies, loaderResults.Policies...)
				vaps = append(vaps, loaderResults.VAPs...)
				vapBindings = append(vapBindings, loaderResults.VAPBindings...)
			}
		} else {
			loaderResults, err := policy.Load(nil, "", path)
			if loaderResults != nil && loaderResults.NonFatalErrors != nil {
				for _, err := range loaderResults.NonFatalErrors {
					log.Log.Error(err.Error, "Non-fatal parsing error for single document")
				}
			}
			if err != nil {
				log.Log.V(3).Info("skipping invalid YAML file", "path", path, "error", err)
			} else {
				policies = append(policies, loaderResults.Policies...)
				vaps = append(vaps, loaderResults.VAPs...)
				vapBindings = append(vapBindings, loaderResults.VAPBindings...)
			}
		}
		for _, policy := range policies {
			if policy.GetNamespace() == "" && policy.GetKind() == "Policy" {
				log.Log.V(3).Info(fmt.Sprintf("Namespace is empty for a namespaced Policy %s. This might cause incorrect report generation.", policy.GetNamespace()))
			}
		}
	}
	return nil, nil, skipInvalidPolicies, nil, policies, vaps, vapBindings, nil
}

func (c *ApplyCommandConfig) initStoreAndClusterClient(store *store.Store, skipInvalidPolicies SkippedInvalidPolicies) (*processor.ResultCounts, []*unstructured.Unstructured, SkippedInvalidPolicies, []engineapi.EngineResponse, dclient.Interface, error) {
	store.SetLocal(true)
	store.SetRegistryAccess(c.RegistryAccess)
	if c.Cluster {
		store.AllowApiCall(true)
	}
	var err error
	var dClient dclient.Interface
	if c.Cluster {
		restConfig, err := config.CreateClientConfigWithContext(c.KubeConfig, c.Context)
		if err != nil {
			return nil, nil, skipInvalidPolicies, nil, nil, err
		}
		kubeClient, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			return nil, nil, skipInvalidPolicies, nil, nil, err
		}
		dynamicClient, err := dynamic.NewForConfig(restConfig)
		if err != nil {
			return nil, nil, skipInvalidPolicies, nil, nil, err
		}
		dClient, err = dclient.NewClient(context.Background(), dynamicClient, kubeClient, 15*time.Minute)
		if err != nil {
			return nil, nil, skipInvalidPolicies, nil, nil, err
		}
	}
	return nil, nil, skipInvalidPolicies, nil, dClient, err
}

func (c *ApplyCommandConfig) cleanPreviousContent(mutateLogPathIsDir bool, skipInvalidPolicies SkippedInvalidPolicies) (*processor.ResultCounts, []*unstructured.Unstructured, SkippedInvalidPolicies, []engineapi.EngineResponse, error) {
	// empty the previous contents of the file just in case if the file already existed before with some content(so as to perform overwrites)
	// the truncation of files for the case when mutateLogPath is dir, is handled under pkg/kyverno/apply/common.go
	if !mutateLogPathIsDir && c.MutateLogPath != "" {
		c.MutateLogPath = filepath.Clean(c.MutateLogPath)
		// Necessary for us to include the file via variable as it is part of the CLI.
		_, err := os.OpenFile(c.MutateLogPath, os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304
		if err != nil {
			return nil, nil, skipInvalidPolicies, nil, fmt.Errorf("failed to truncate the existing file at %s (%w)", c.MutateLogPath, err)
		}
	}
	return nil, nil, skipInvalidPolicies, nil, nil
}

func (c *ApplyCommandConfig) checkArguments() (*processor.ResultCounts, []*unstructured.Unstructured, SkippedInvalidPolicies, []engineapi.EngineResponse, error) {
	var skipInvalidPolicies SkippedInvalidPolicies
	if c.ValuesFile != "" && c.Variables != nil {
		return nil, nil, skipInvalidPolicies, nil, fmt.Errorf("pass the values either using set flag or values_file flag")
	}
	if len(c.PolicyPaths) == 0 {
		return nil, nil, skipInvalidPolicies, nil, fmt.Errorf("require policy")
	}
	if (len(c.PolicyPaths) > 0 && c.PolicyPaths[0] == "-") && len(c.ResourcePaths) > 0 && c.ResourcePaths[0] == "-" {
		return nil, nil, skipInvalidPolicies, nil, fmt.Errorf("a stdin pipe can be used for either policies or resources, not both")
	}
	if len(c.ResourcePaths) == 0 && !c.Cluster {
		return nil, nil, skipInvalidPolicies, nil, fmt.Errorf("resource file(s) or cluster required")
	}
	return nil, nil, skipInvalidPolicies, nil, nil
}

type WarnExitCodeError struct {
	ExitCode int
}

func (w WarnExitCodeError) Error() string {
	return fmt.Sprintf("exit as warnExitCode is %d", w.ExitCode)
}

func exit(out io.Writer, rc *processor.ResultCounts, warnExitCode int, warnNoPassed bool) error {
	if rc.Fail > 0 {
		return fmt.Errorf("exit as there are policy violations")
	} else if rc.Error > 0 {
		return fmt.Errorf("exit as there are policy errors")
	} else if rc.Warn > 0 && warnExitCode != 0 {
		fmt.Printf("exit as warnExitCode is %d", warnExitCode)
		return WarnExitCodeError{
			ExitCode: warnExitCode,
		}
	} else if rc.Pass == 0 && warnNoPassed {
		fmt.Println(out, "exit as no objects satisfied policy")
		return WarnExitCodeError{
			ExitCode: warnExitCode,
		}
	}
	return nil
}
