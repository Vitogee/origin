package validation

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/validation"
	kvalidation "k8s.io/kubernetes/pkg/util/validation"
	"k8s.io/kubernetes/pkg/util/validation/field"

	oapi "github.com/openshift/origin/pkg/api"
	buildapi "github.com/openshift/origin/pkg/build/api"
	buildutil "github.com/openshift/origin/pkg/build/util"
	imageapi "github.com/openshift/origin/pkg/image/api"
)

// ValidateBuild tests required fields for a Build.
func ValidateBuild(build *buildapi.Build) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validation.ValidateObjectMeta(&build.ObjectMeta, true, validation.NameIsDNSSubdomain, field.NewPath("metadata"))...)
	allErrs = append(allErrs, validateBuildSpec(&build.Spec, field.NewPath("spec"))...)
	return allErrs
}

func ValidateBuildUpdate(build *buildapi.Build, older *buildapi.Build) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validation.ValidateObjectMetaUpdate(&build.ObjectMeta, &older.ObjectMeta, field.NewPath("metadata"))...)

	allErrs = append(allErrs, ValidateBuild(build)...)

	if buildutil.IsBuildComplete(older) && older.Status.Phase != build.Status.Phase {
		allErrs = append(allErrs, field.Invalid(field.NewPath("status", "phase"), build.Status.Phase, "phase cannot be updated from a terminal state"))
	}
	if !kapi.Semantic.DeepEqual(build.Spec, older.Spec) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec"), "content of spec is not printed out, please refer to the \"details\"", "spec is immutable"))
	}

	return allErrs
}

// refKey returns a key for the given ObjectReference. If the ObjectReference
// doesn't include a namespace, the passed in namespace is used for the reference
func refKey(namespace string, ref *kapi.ObjectReference) string {
	if ref == nil || ref.Kind != "ImageStreamTag" {
		return "nil"
	}
	ns := ref.Namespace
	if ns == "" {
		ns = namespace
	}
	return fmt.Sprintf("%s/%s", ns, ref.Name)
}

// ValidateBuildConfig tests required fields for a Build.
func ValidateBuildConfig(config *buildapi.BuildConfig) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validation.ValidateObjectMeta(&config.ObjectMeta, true, validation.NameIsDNSSubdomain, field.NewPath("metadata"))...)

	// image change triggers that refer
	fromRefs := map[string]struct{}{}
	specPath := field.NewPath("spec")
	triggersPath := specPath.Child("triggers")
	for i, trg := range config.Spec.Triggers {
		allErrs = append(allErrs, validateTrigger(&trg, triggersPath.Index(i))...)
		if trg.Type != buildapi.ImageChangeBuildTriggerType || trg.ImageChange == nil {
			continue
		}
		from := trg.ImageChange.From
		if from == nil {
			from = buildutil.GetImageStreamForStrategy(config.Spec.Strategy)
		}
		fromKey := refKey(config.Namespace, from)
		_, exists := fromRefs[fromKey]
		if exists {
			allErrs = append(allErrs, field.Invalid(triggersPath, config.Spec.Triggers, "multiple ImageChange triggers refer to the same image stream tag"))
		}
		fromRefs[fromKey] = struct{}{}
	}

	allErrs = append(allErrs, validateBuildSpec(&config.Spec.BuildSpec, specPath)...)

	// validate ImageChangeTriggers of DockerStrategy builds
	strategy := config.Spec.BuildSpec.Strategy
	if strategy.DockerStrategy != nil && strategy.DockerStrategy.From == nil {
		for i, trigger := range config.Spec.Triggers {
			if trigger.Type == buildapi.ImageChangeBuildTriggerType && (trigger.ImageChange == nil || trigger.ImageChange.From == nil) {
				allErrs = append(allErrs, field.Required(triggersPath.Index(i).Child("imageChange", "from")))
			}
		}
	}

	return allErrs
}

func ValidateBuildConfigUpdate(config *buildapi.BuildConfig, older *buildapi.BuildConfig) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validation.ValidateObjectMetaUpdate(&config.ObjectMeta, &older.ObjectMeta, field.NewPath("metadata"))...)
	allErrs = append(allErrs, ValidateBuildConfig(config)...)
	return allErrs
}

// ValidateBuildRequest validates a BuildRequest object
func ValidateBuildRequest(request *buildapi.BuildRequest) field.ErrorList {
	return validation.ValidateObjectMeta(&request.ObjectMeta, true, oapi.MinimalNameRequirements, field.NewPath("metadata"))
}

func validateBuildSpec(spec *buildapi.BuildSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	s := spec.Strategy

	if s.CustomStrategy == nil && spec.Source.Git == nil && spec.Source.Binary == nil && spec.Source.Dockerfile == nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("source"), spec.Source, "must provide a value for at least one of source, binary, or dockerfile"))
	}

	allErrs = append(allErrs, validateSource(&spec.Source, s.CustomStrategy != nil, s.DockerStrategy != nil, fldPath.Child("source"))...)

	if spec.CompletionDeadlineSeconds != nil {
		if *spec.CompletionDeadlineSeconds <= 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("completionDeadlineSeconds"), spec.CompletionDeadlineSeconds, "completionDeadlineSeconds must be a positive integer greater than 0"))
		}
	}

	allErrs = append(allErrs, validateOutput(&spec.Output, fldPath.Child("output"))...)
	allErrs = append(allErrs, validateStrategy(&spec.Strategy, fldPath.Child("strategy"))...)

	// TODO: validate resource requirements (prereq: https://github.com/kubernetes/kubernetes/pull/7059)
	return allErrs
}

const maxDockerfileLengthBytes = 60 * 1000

func hasProxy(source *buildapi.GitBuildSource) bool {
	return len(source.HTTPProxy) > 0 || len(source.HTTPSProxy) > 0
}

func validateSource(input *buildapi.BuildSource, isCustomStrategy, isDockerStrategy bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	// Ensure that Git and Binary source types are mutually exclusive.
	if input.Git != nil && input.Binary != nil && !isCustomStrategy {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("git"), "", "may not be set when binary is also set"))
		allErrs = append(allErrs, field.Invalid(fldPath.Child("binary"), "", "may not be set when git is also set"))
		return allErrs
	}

	// Validate individual source type details
	if input.Git != nil {
		allErrs = append(allErrs, validateGitSource(input.Git, fldPath.Child("git"))...)
	}
	if input.Binary != nil {
		allErrs = append(allErrs, validateBinarySource(input.Binary, fldPath.Child("binary"))...)
	}
	if input.Dockerfile != nil {
		allErrs = append(allErrs, validateDockerfile(*input.Dockerfile, fldPath.Child("dockerfile"))...)
	}
	if input.Images != nil {
		for i, image := range input.Images {
			allErrs = append(allErrs, validateImageSource(image, fldPath.Child("images").Index(i))...)
		}
	}

	allErrs = append(allErrs, validateSecrets(input.Secrets, isDockerStrategy, fldPath.Child("secrets"))...)

	allErrs = append(allErrs, validateSecretRef(input.SourceSecret, fldPath.Child("sourceSecret"))...)

	if len(input.ContextDir) != 0 {
		cleaned := path.Clean(input.ContextDir)
		if strings.HasPrefix(cleaned, "..") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("contextDir"), input.ContextDir, "context dir must not be a relative path"))
		} else {
			if cleaned == "." {
				cleaned = ""
			}
			input.ContextDir = cleaned
		}
	}

	return allErrs
}

func validateDockerfile(dockerfile string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(dockerfile) > maxDockerfileLengthBytes {
		allErrs = append(allErrs, field.Invalid(fldPath, "", fmt.Sprintf("must be smaller than %d bytes", maxDockerfileLengthBytes)))
	}
	return allErrs
}

func validateSecretRef(ref *kapi.LocalObjectReference, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if ref == nil {
		return allErrs
	}
	if len(ref.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name")))
	}
	return allErrs
}

func isHTTPScheme(in string) bool {
	u, err := url.Parse(in)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func validateGitSource(git *buildapi.GitBuildSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(git.URI) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("uri")))
	} else if !isValidURL(git.URI) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("uri"), git.URI, "uri is not a valid url"))
	}
	if len(git.HTTPProxy) != 0 && !isValidURL(git.HTTPProxy) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("httpproxy"), git.HTTPProxy, "proxy is not a valid url"))
	}
	if len(git.HTTPSProxy) != 0 && !isValidURL(git.HTTPSProxy) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("httpsproxy"), git.HTTPSProxy, "proxy is not a valid url"))
	}
	if hasProxy(git) && !isHTTPScheme(git.URI) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("uri"), git.URI, "only http:// and https:// GIT protocols are allowed with HTTP or HTTPS proxy set"))
	}
	return allErrs
}

func validateSecrets(secrets []buildapi.SecretBuildSource, isDockerStrategy bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, s := range secrets {
		if len(s.Secret.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Index(i).Child("secret")))
		}
		if ok, _ := validation.ValidateSecretName(s.Secret.Name, false); !ok {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("secret"), s, "must be valid secret name"))
		}
		if strings.HasPrefix(path.Clean(s.DestinationDir), "..") {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("destinationDir"), s.DestinationDir, "destination dir cannot start with '..'"))
		}
		if isDockerStrategy && filepath.IsAbs(s.DestinationDir) {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("destinationDir"), s.DestinationDir, "for the docker strategy the destinationDir has to be relative path"))
		}
	}
	return allErrs
}

func validateImageSource(imageSource buildapi.ImageSource, fldPath *field.Path) field.ErrorList {
	allErrs := validateFromImageReference(&imageSource.From, fldPath.Child("from"))
	if imageSource.PullSecret != nil {
		allErrs = append(allErrs, validateSecretRef(imageSource.PullSecret, fldPath.Child("pullSecret"))...)
	}
	if len(imageSource.Paths) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("paths")))
	}
	for i, path := range imageSource.Paths {
		allErrs = append(allErrs, validateImageSourcePath(path, fldPath.Child("paths").Index(i))...)

	}
	return allErrs
}

func validateImageSourcePath(imagePath buildapi.ImageSourcePath, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(imagePath.SourcePath) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("sourcePath")))
	}
	if len(imagePath.DestinationDir) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("destinationDir")))
	}
	if !filepath.IsAbs(imagePath.SourcePath) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("sourcePath"), imagePath.SourcePath, "must be an absolute path"))
	}
	if filepath.IsAbs(imagePath.DestinationDir) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("destinationDir"), imagePath.DestinationDir, "must be a relative path"))
	}
	if strings.HasPrefix(path.Clean(imagePath.DestinationDir), "..") {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("destinationDir"), imagePath.DestinationDir, "destination dir cannot start with '..'"))
	}
	return allErrs
}

func validateBinarySource(source *buildapi.BinaryBuildSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(source.AsFile) != 0 {
		cleaned := strings.TrimPrefix(path.Clean(source.AsFile), "/")
		if len(cleaned) == 0 || cleaned == "." || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/") || strings.Contains(cleaned, "\\") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("asFile"), source.AsFile, "file name may not contain slashes or relative path segments and must be a valid POSIX filename"))
		} else {
			source.AsFile = cleaned
		}
	}
	return allErrs
}

func validateToImageReference(reference *kapi.ObjectReference, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	kind, name, namespace := reference.Kind, reference.Name, reference.Namespace
	switch kind {
	case "ImageStreamTag":
		if len(name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("name")))
		} else if _, _, ok := imageapi.SplitImageStreamTag(name); !ok {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), name, "ImageStreamTag object references must be in the form <name>:<tag>"))
		}
		if len(namespace) != 0 && !kvalidation.IsDNS1123Subdomain(namespace) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), namespace, "namespace must be a valid subdomain"))
		}

	case "DockerImage":
		if len(namespace) != 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), namespace, "namespace is not valid when used with a 'DockerImage'"))
		}
		if _, err := imageapi.ParseDockerImageReference(name); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), name, fmt.Sprintf("name is not a valid Docker pull specification: %v", err)))
		}
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("kind")))
	default:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("kind"), kind, "the target of build output must be an 'ImageStreamTag' or 'DockerImage'"))

	}
	return allErrs
}

func validateFromImageReference(reference *kapi.ObjectReference, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	kind, name, namespace := reference.Kind, reference.Name, reference.Namespace
	switch kind {
	case "ImageStreamTag":
		if len(name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("name")))
		} else if _, _, ok := imageapi.SplitImageStreamTag(name); !ok {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), name, "ImageStreamTag object references must be in the form <name>:<tag>"))
		}

		if len(namespace) != 0 && !kvalidation.IsDNS1123Subdomain(namespace) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), namespace, "namespace must be a valid subdomain"))
		}

	case "DockerImage":
		if len(namespace) != 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), namespace, "namespace is not valid when used with a 'DockerImage'"))
		}
		if len(name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("name")))
		} else if _, err := imageapi.ParseDockerImageReference(name); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), name, fmt.Sprintf("name is not a valid Docker pull specification: %v", err)))
		}
	case "ImageStreamImage":
		if len(name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("name")))
		}
		if len(namespace) != 0 && !kvalidation.IsDNS1123Subdomain(namespace) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), namespace, "namespace must be a valid subdomain"))
		}
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("kind")))
	default:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("kind"), kind, "the source of a builder image must be an 'ImageStreamTag', 'ImageStreamImage', or 'DockerImage'"))

	}
	return allErrs
}

func validateOutput(output *buildapi.BuildOutput, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	// TODO: make part of a generic ValidateObjectReference method upstream.
	if output.To != nil {
		allErrs = append(allErrs, validateToImageReference(output.To, fldPath.Child("to"))...)
	}

	allErrs = append(allErrs, validateSecretRef(output.PushSecret, fldPath.Child("pushSecret"))...)

	return allErrs
}

func validateStrategy(strategy *buildapi.BuildStrategy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	strategyCount := 0
	if strategy.SourceStrategy != nil {
		strategyCount++
	}
	if strategy.DockerStrategy != nil {
		strategyCount++
	}
	if strategy.CustomStrategy != nil {
		strategyCount++
	}
	if strategyCount != 1 {
		return append(allErrs, field.Invalid(fldPath, strategy, "must provide a value for exactly one of sourceStrategy, customStrategy, or dockerStrategy"))
	}

	if strategy.SourceStrategy != nil {
		allErrs = append(allErrs, validateSourceStrategy(strategy.SourceStrategy, fldPath.Child("sourceStrategy"))...)
	}
	if strategy.DockerStrategy != nil {
		allErrs = append(allErrs, validateDockerStrategy(strategy.DockerStrategy, fldPath.Child("dockerStrategy"))...)
	}
	if strategy.CustomStrategy != nil {
		allErrs = append(allErrs, validateCustomStrategy(strategy.CustomStrategy, fldPath.Child("customStrategy"))...)
	}

	return allErrs
}

func validateDockerStrategy(strategy *buildapi.DockerBuildStrategy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if strategy.From != nil {
		allErrs = append(allErrs, validateFromImageReference(strategy.From, fldPath.Child("from"))...)
	}

	allErrs = append(allErrs, validateSecretRef(strategy.PullSecret, fldPath.Child("pullSecret"))...)

	if len(strategy.DockerfilePath) != 0 {
		cleaned := path.Clean(strategy.DockerfilePath)
		switch {
		case strings.HasPrefix(cleaned, "/"):
			allErrs = append(allErrs, field.Invalid(fldPath.Child("dockerfilePath"), strategy.DockerfilePath, "dockerfilePath must not be an absolute path"))
		case strings.HasPrefix(cleaned, ".."):
			allErrs = append(allErrs, field.Invalid(fldPath.Child("dockerfilePath"), strategy.DockerfilePath, "dockerfilePath must not start with .."))
		default:
			if cleaned == "." {
				cleaned = ""
			}
			strategy.DockerfilePath = cleaned
		}
	}

	return allErrs
}

func validateSourceStrategy(strategy *buildapi.SourceBuildStrategy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateFromImageReference(&strategy.From, fldPath.Child("from"))...)
	allErrs = append(allErrs, validateSecretRef(strategy.PullSecret, fldPath.Child("pullSecret"))...)
	return allErrs
}

func validateCustomStrategy(strategy *buildapi.CustomBuildStrategy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateFromImageReference(&strategy.From, fldPath.Child("from"))...)
	allErrs = append(allErrs, validateSecretRef(strategy.PullSecret, fldPath.Child("pullSecret"))...)
	return allErrs
}

func validateTrigger(trigger *buildapi.BuildTriggerPolicy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(trigger.Type) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("type")))
		return allErrs
	}

	// Validate each trigger type
	switch trigger.Type {
	case buildapi.GitHubWebHookBuildTriggerType:
		if trigger.GitHubWebHook == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("github")))
		} else {
			allErrs = append(allErrs, validateWebHook(trigger.GitHubWebHook, fldPath.Child("github"))...)
		}
	case buildapi.GenericWebHookBuildTriggerType:
		if trigger.GenericWebHook == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("generic")))
		} else {
			allErrs = append(allErrs, validateWebHook(trigger.GenericWebHook, fldPath.Child("generic"))...)
		}
	case buildapi.ImageChangeBuildTriggerType:
		if trigger.ImageChange == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("imageChange")))
			break
		}
		if trigger.ImageChange.From == nil {
			break
		}
		if kind := trigger.ImageChange.From.Kind; kind != "ImageStreamTag" {
			invalidKindErr := field.Invalid(
				fldPath.Child("imageChange").Child("from").Child("kind"),
				kind,
				"only an ImageStreamTag type of reference is allowed in an ImageChange trigger.")
			allErrs = append(allErrs, invalidKindErr)
			break
		}
		allErrs = append(allErrs, validateFromImageReference(trigger.ImageChange.From, fldPath.Child("from"))...)
	case buildapi.ConfigChangeBuildTriggerType:
		// doesn't require additional validation
	default:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("type"), trigger.Type, "invalid trigger type"))
	}
	return allErrs
}

func validateWebHook(webHook *buildapi.WebHookTrigger, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(webHook.Secret) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("secret")))
	}
	return allErrs
}

func isValidURL(uri string) bool {
	_, err := url.Parse(uri)
	return err == nil
}

func ValidateBuildLogOptions(opts *buildapi.BuildLogOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	// TODO: Replace by validating PodLogOptions via BuildLogOptions once it's bundled in
	popts := buildapi.BuildToPodLogOptions(opts)
	if errs := validation.ValidatePodLogOptions(popts); len(errs) > 0 {
		allErrs = append(allErrs, errs...)
	}

	if opts.Version != nil && *opts.Version <= 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("version"), *opts.Version, "build version must be greater than 0"))
	}
	if opts.Version != nil && opts.Previous {
		allErrs = append(allErrs, field.Invalid(field.NewPath("previous"), opts.Previous, "cannot use previous when a version is specified"))
	}
	return allErrs
}
