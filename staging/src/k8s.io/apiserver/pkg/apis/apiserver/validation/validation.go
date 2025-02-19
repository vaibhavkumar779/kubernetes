/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validation

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "k8s.io/api/authorization/v1"
	"k8s.io/api/authorization/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	api "k8s.io/apiserver/pkg/apis/apiserver"
	authorizationcel "k8s.io/apiserver/pkg/authorization/cel"
	"k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/apiserver/pkg/features"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/util/cert"
)

const (
	atLeastOneRequiredErrFmt = "at least one %s is required"
)

var (
	root = field.NewPath("jwt")
)

// ValidateAuthenticationConfiguration validates a given AuthenticationConfiguration.
func ValidateAuthenticationConfiguration(c *api.AuthenticationConfiguration) field.ErrorList {
	var allErrs field.ErrorList

	// This stricter validation is solely based on what the current implementation supports.
	// TODO(aramase): when StructuredAuthenticationConfiguration feature gate is added and wired up,
	// relax this check to allow 0 authenticators. This will allow us to support the case where
	// API server is initially configured with no authenticators and then authenticators are added
	// later via dynamic config.
	if len(c.JWT) == 0 {
		allErrs = append(allErrs, field.Required(root, fmt.Sprintf(atLeastOneRequiredErrFmt, root)))
		return allErrs
	}

	// This stricter validation is because the --oidc-* flag option is singular.
	// TODO(aramase): when StructuredAuthenticationConfiguration feature gate is added and wired up,
	// remove the 1 authenticator limit check and add set the limit to 64.
	if len(c.JWT) > 1 {
		allErrs = append(allErrs, field.TooMany(root, len(c.JWT), 1))
		return allErrs
	}

	// TODO(aramase): right now we only support a single JWT authenticator as
	// this is wired to the --oidc-* flags. When StructuredAuthenticationConfiguration
	// feature gate is added and wired up, we will remove the 1 authenticator limit
	// check and add validation for duplicate issuers.
	for i, a := range c.JWT {
		fldPath := root.Index(i)
		allErrs = append(allErrs, validateJWTAuthenticator(a, fldPath)...)
	}

	return allErrs
}

// ValidateJWTAuthenticator validates a given JWTAuthenticator.
// This is exported for use in oidc package.
func ValidateJWTAuthenticator(authenticator api.JWTAuthenticator) field.ErrorList {
	return validateJWTAuthenticator(authenticator, nil)
}

func validateJWTAuthenticator(authenticator api.JWTAuthenticator, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateIssuer(authenticator.Issuer, fldPath.Child("issuer"))...)
	allErrs = append(allErrs, validateClaimValidationRules(authenticator.ClaimValidationRules, fldPath.Child("claimValidationRules"))...)
	allErrs = append(allErrs, validateClaimMappings(authenticator.ClaimMappings, fldPath.Child("claimMappings"))...)

	return allErrs
}

func validateIssuer(issuer api.Issuer, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	allErrs = append(allErrs, validateURL(issuer.URL, fldPath.Child("url"))...)
	allErrs = append(allErrs, validateAudiences(issuer.Audiences, fldPath.Child("audiences"))...)
	allErrs = append(allErrs, validateCertificateAuthority(issuer.CertificateAuthority, fldPath.Child("certificateAuthority"))...)

	return allErrs
}

func validateURL(issuerURL string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(issuerURL) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "URL is required"))
		return allErrs
	}

	u, err := url.Parse(issuerURL)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, issuerURL, err.Error()))
		return allErrs
	}
	if u.Scheme != "https" {
		allErrs = append(allErrs, field.Invalid(fldPath, issuerURL, "URL scheme must be https"))
	}
	if u.User != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, issuerURL, "URL must not contain a username or password"))
	}
	if len(u.RawQuery) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, issuerURL, "URL must not contain a query"))
	}
	if len(u.Fragment) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, issuerURL, "URL must not contain a fragment"))
	}

	return allErrs
}

func validateAudiences(audiences []string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(audiences) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, fmt.Sprintf(atLeastOneRequiredErrFmt, fldPath)))
		return allErrs
	}
	// This stricter validation is because the --oidc-client-id flag option is singular.
	// This will be removed when we support multiple audiences with the StructuredAuthenticationConfiguration feature gate.
	if len(audiences) > 1 {
		allErrs = append(allErrs, field.TooMany(fldPath, len(audiences), 1))
		return allErrs
	}

	for i, audience := range audiences {
		fldPath := fldPath.Index(i)
		if len(audience) == 0 {
			allErrs = append(allErrs, field.Required(fldPath, "audience can't be empty"))
		}
	}

	return allErrs
}

func validateCertificateAuthority(certificateAuthority string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(certificateAuthority) == 0 {
		return allErrs
	}
	_, err := cert.NewPoolFromBytes([]byte(certificateAuthority))
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, "<omitted>", err.Error()))
	}

	return allErrs
}

func validateClaimValidationRules(rules []api.ClaimValidationRule, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	seenClaims := sets.NewString()
	for i, rule := range rules {
		fldPath := fldPath.Index(i)

		if len(rule.Claim) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("claim"), "claim name is required"))
			continue
		}

		if seenClaims.Has(rule.Claim) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Child("claim"), rule.Claim))
			continue
		}
		seenClaims.Insert(rule.Claim)
	}

	return allErrs
}

func validateClaimMappings(m api.ClaimMappings, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if len(m.Username.Claim) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("username", "claim"), "claim name is required"))
	}
	// TODO(aramase): when Expression is added to PrefixedClaimOrExpression, check prefix and expression are not both set.
	if m.Username.Prefix == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("username", "prefix"), "prefix is required"))
	}
	if len(m.Groups.Claim) > 0 && m.Groups.Prefix == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("groups", "prefix"), "prefix is required when claim is set"))
	}
	if m.Groups.Prefix != nil && len(m.Groups.Claim) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("groups", "claim"), "non-empty claim name is required when prefix is set"))
	}

	return allErrs
}

// ValidateAuthorizationConfiguration validates a given AuthorizationConfiguration.
func ValidateAuthorizationConfiguration(fldPath *field.Path, c *api.AuthorizationConfiguration, knownTypes sets.String, repeatableTypes sets.String) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(c.Authorizers) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("authorizers"), "at least one authorization mode must be defined"))
	}

	seenAuthorizerTypes := sets.NewString()
	seenAuthorizerNames := sets.NewString()
	for i, a := range c.Authorizers {
		fldPath := fldPath.Child("authorizers").Index(i)
		aType := string(a.Type)
		if aType == "" {
			allErrs = append(allErrs, field.Required(fldPath.Child("type"), ""))
			continue
		}
		if !knownTypes.Has(aType) {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("type"), aType, knownTypes.List()))
			continue
		}
		if seenAuthorizerTypes.Has(aType) && !repeatableTypes.Has(aType) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Child("type"), aType))
			continue
		}
		seenAuthorizerTypes.Insert(aType)

		if len(a.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
		} else if seenAuthorizerNames.Has(a.Name) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Child("name"), a.Name))
		} else if errs := utilvalidation.IsDNS1123Subdomain(a.Name); len(errs) != 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), a.Name, fmt.Sprintf("authorizer name is invalid: %s", strings.Join(errs, ", "))))
		}
		seenAuthorizerNames.Insert(a.Name)

		switch a.Type {
		case api.TypeWebhook:
			if a.Webhook == nil {
				allErrs = append(allErrs, field.Required(fldPath.Child("webhook"), "required when type=Webhook"))
				continue
			}
			allErrs = append(allErrs, ValidateWebhookConfiguration(fldPath, a.Webhook)...)
		default:
			if a.Webhook != nil {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("webhook"), "non-null", "may only be specified when type=Webhook"))
			}
		}
	}

	return allErrs
}

func ValidateWebhookConfiguration(fldPath *field.Path, c *api.WebhookConfiguration) field.ErrorList {
	allErrs := field.ErrorList{}

	if c.Timeout.Duration == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("timeout"), ""))
	} else if c.Timeout.Duration > 30*time.Second || c.Timeout.Duration < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("timeout"), c.Timeout.Duration.String(), "must be > 0s and <= 30s"))
	}

	if c.AuthorizedTTL.Duration == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("authorizedTTL"), ""))
	} else if c.AuthorizedTTL.Duration < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("authorizedTTL"), c.AuthorizedTTL.Duration.String(), "must be > 0s"))
	}

	if c.UnauthorizedTTL.Duration == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("unauthorizedTTL"), ""))
	} else if c.UnauthorizedTTL.Duration < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("unauthorizedTTL"), c.UnauthorizedTTL.Duration.String(), "must be > 0s"))
	}

	switch c.SubjectAccessReviewVersion {
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("subjectAccessReviewVersion"), ""))
	case "v1":
		_ = &v1.SubjectAccessReview{}
	case "v1beta1":
		_ = &v1beta1.SubjectAccessReview{}
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("subjectAccessReviewVersion"), c.SubjectAccessReviewVersion, []string{"v1", "v1beta1"}))
	}

	switch c.MatchConditionSubjectAccessReviewVersion {
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("matchConditionSubjectAccessReviewVersion"), ""))
	case "v1":
		_ = &v1.SubjectAccessReview{}
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("matchConditionSubjectAccessReviewVersion"), c.MatchConditionSubjectAccessReviewVersion, []string{"v1"}))
	}

	switch c.FailurePolicy {
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("failurePolicy"), ""))
	case api.FailurePolicyNoOpinion, api.FailurePolicyDeny:
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("failurePolicy"), c.FailurePolicy, []string{"NoOpinion", "Deny"}))
	}

	switch c.ConnectionInfo.Type {
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("connectionInfo", "type"), ""))
	case api.AuthorizationWebhookConnectionInfoTypeInCluster:
		if c.ConnectionInfo.KubeConfigFile != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("connectionInfo", "kubeConfigFile"), *c.ConnectionInfo.KubeConfigFile, "can only be set when type=KubeConfigFile"))
		}
	case api.AuthorizationWebhookConnectionInfoTypeKubeConfigFile:
		if c.ConnectionInfo.KubeConfigFile == nil || *c.ConnectionInfo.KubeConfigFile == "" {
			allErrs = append(allErrs, field.Required(fldPath.Child("connectionInfo", "kubeConfigFile"), ""))
		} else if !filepath.IsAbs(*c.ConnectionInfo.KubeConfigFile) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("connectionInfo", "kubeConfigFile"), *c.ConnectionInfo.KubeConfigFile, "must be an absolute path"))
		} else if info, err := os.Stat(*c.ConnectionInfo.KubeConfigFile); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("connectionInfo", "kubeConfigFile"), *c.ConnectionInfo.KubeConfigFile, fmt.Sprintf("error loading file: %v", err)))
		} else if !info.Mode().IsRegular() {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("connectionInfo", "kubeConfigFile"), *c.ConnectionInfo.KubeConfigFile, "must be a regular file"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("connectionInfo", "type"), c.ConnectionInfo, []string{api.AuthorizationWebhookConnectionInfoTypeInCluster, api.AuthorizationWebhookConnectionInfoTypeKubeConfigFile}))
	}

	_, errs := compileMatchConditions(c.MatchConditions, fldPath, utilfeature.DefaultFeatureGate.Enabled(features.StructuredAuthorizationConfiguration))
	allErrs = append(allErrs, errs...)

	return allErrs
}

// ValidateAndCompileMatchConditions validates a given webhook's matchConditions.
// This is exported for use in authz package.
func ValidateAndCompileMatchConditions(matchConditions []api.WebhookMatchCondition) (*authorizationcel.CELMatcher, field.ErrorList) {
	return compileMatchConditions(matchConditions, nil, utilfeature.DefaultFeatureGate.Enabled(features.StructuredAuthorizationConfiguration))
}

func compileMatchConditions(matchConditions []api.WebhookMatchCondition, fldPath *field.Path, structuredAuthzFeatureEnabled bool) (*authorizationcel.CELMatcher, field.ErrorList) {
	var allErrs field.ErrorList
	// should fail when match conditions are used without feature enabled
	if len(matchConditions) > 0 && !structuredAuthzFeatureEnabled {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("matchConditions"), "", "matchConditions are not supported when StructuredAuthorizationConfiguration feature gate is disabled"))
	}
	if len(matchConditions) > 64 {
		allErrs = append(allErrs, field.TooMany(fldPath.Child("matchConditions"), len(matchConditions), 64))
		return nil, allErrs
	}

	compiler := authorizationcel.NewCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	seenExpressions := sets.NewString()
	var compilationResults []authorizationcel.CompilationResult

	for i, condition := range matchConditions {
		fldPath := fldPath.Child("matchConditions").Index(i).Child("expression")
		if len(strings.TrimSpace(condition.Expression)) == 0 {
			allErrs = append(allErrs, field.Required(fldPath, ""))
			continue
		}
		if seenExpressions.Has(condition.Expression) {
			allErrs = append(allErrs, field.Duplicate(fldPath, condition.Expression))
			continue
		}
		seenExpressions.Insert(condition.Expression)
		compilationResult, err := compileMatchConditionsExpression(fldPath, compiler, condition.Expression)
		if err != nil {
			allErrs = append(allErrs, err)
			continue
		}
		compilationResults = append(compilationResults, compilationResult)
	}
	if len(compilationResults) == 0 {
		return nil, allErrs
	}
	return &authorizationcel.CELMatcher{
		CompilationResults: compilationResults,
	}, allErrs
}

func compileMatchConditionsExpression(fldPath *field.Path, compiler authorizationcel.Compiler, expression string) (authorizationcel.CompilationResult, *field.Error) {
	authzExpression := &authorizationcel.SubjectAccessReviewMatchCondition{
		Expression: expression,
	}
	compilationResult, err := compiler.CompileCELExpression(authzExpression)
	if err != nil {
		return compilationResult, convertCELErrorToValidationError(fldPath, authzExpression, err)
	}
	return compilationResult, nil
}

func convertCELErrorToValidationError(fldPath *field.Path, expression authorizationcel.ExpressionAccessor, err error) *field.Error {
	var celErr *cel.Error
	if errors.As(err, &celErr) {
		switch celErr.Type {
		case cel.ErrorTypeRequired:
			return field.Required(fldPath, celErr.Detail)
		case cel.ErrorTypeInvalid:
			return field.Invalid(fldPath, expression.GetExpression(), celErr.Detail)
		default:
			return field.InternalError(fldPath, celErr)
		}
	}
	return field.InternalError(fldPath, fmt.Errorf("error is not cel error: %w", err))
}
