package authorizationpolicy

import (
	"context"
	"fmt"
	"golang.org/x/exp/slices"
	"strings"

	"github.com/kyma-project/api-gateway/internal/helpers"
	"github.com/kyma-project/api-gateway/internal/processing/default_domain"
	networkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	gatewayv2alpha1 "github.com/kyma-project/api-gateway/apis/gateway/v2alpha1"

	"github.com/kyma-project/api-gateway/internal/builders"
	"github.com/kyma-project/api-gateway/internal/processing"
	"github.com/kyma-project/api-gateway/internal/processing/hashbasedstate"
	"istio.io/api/security/v1beta1"
	securityv1beta1 "istio.io/client-go/pkg/apis/security/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	audienceKey string = "request.auth.claims[aud]"
)

var (
	defaultScopeKeys = []string{"request.auth.claims[scp]", "request.auth.claims[scope]", "request.auth.claims[scopes]"}
)

// Creator provides the creation of AuthorizationPolicy using the configuration in the given APIRule.
type Creator interface {
	Create(ctx context.Context, client client.Client, api *gatewayv2alpha1.APIRule) (hashbasedstate.Desired, error)
}

type creator struct {
	// Controls that requests to Ory Oathkeeper are also permitted when
	// migrating from APIRule v1beta1 to v2alpha1.
	oryPassthrough bool
	gateway        *networkingv1beta1.Gateway
}

// Create returns the AuthorizationPolicy using the configuration of the APIRule.
func (r creator) Create(ctx context.Context, client client.Client, apiRule *gatewayv2alpha1.APIRule) (hashbasedstate.Desired, error) {
	state := hashbasedstate.NewDesired()
	for _, rule := range apiRule.Spec.Rules {
		notPaths := generateNotPaths(apiRule.Spec.Rules, rule)
		aps, err := r.generateAuthorizationPolicies(ctx, client, apiRule, rule, notPaths)
		if err != nil {
			return state, err
		}

		for _, ap := range aps.Items {
			h := hashbasedstate.NewAuthorizationPolicy(ap)
			err := state.Add(&h)

			if err != nil {
				return state, err
			}
		}
	}
	return state, nil
}

func (r creator) generateAuthorizationPolicies(ctx context.Context, client client.Client, api *gatewayv2alpha1.APIRule, rule gatewayv2alpha1.Rule, notPaths []string) (*securityv1beta1.AuthorizationPolicyList, error) {
	authorizationPolicyList := securityv1beta1.AuthorizationPolicyList{}

	var jwtAuthorizations []*gatewayv2alpha1.JwtAuthorization

	baseHashIndex := 0
	switch {
	case rule.Jwt != nil:
		jwtAuthorizations = append(jwtAuthorizations, rule.Jwt.Authorizations...)
	case rule.ExtAuth != nil:
		baseHashIndex = len(rule.ExtAuth.ExternalAuthorizers)
		if rule.ExtAuth.Restrictions != nil {
			jwtAuthorizations = append(jwtAuthorizations, rule.ExtAuth.Restrictions.Authorizations...)
		}
		policies, err := r.generateExtAuthAuthorizationPolicies(ctx, client, api, rule, notPaths)
		if err != nil {
			return &authorizationPolicyList, err
		}

		authorizationPolicyList.Items = append(authorizationPolicyList.Items, policies...)
	}

	if len(jwtAuthorizations) == 0 {
		ap, err := r.generateAuthorizationPolicyForEmptyAuthorizations(ctx, client, api, rule, notPaths)
		if err != nil {
			return &authorizationPolicyList, err
		}

		err = hashbasedstate.AddLabelsToAuthorizationPolicy(ap, baseHashIndex)
		if err != nil {
			return &authorizationPolicyList, err
		}

		authorizationPolicyList.Items = append(authorizationPolicyList.Items, ap)
	} else {
		for indexInYaml, authorization := range jwtAuthorizations {
			ap, err := r.generateAuthorizationPolicy(ctx, client, api, rule, authorization, notPaths)
			if err != nil {
				return &authorizationPolicyList, err
			}

			err = hashbasedstate.AddLabelsToAuthorizationPolicy(ap, indexInYaml+baseHashIndex)
			if err != nil {
				return &authorizationPolicyList, err
			}

			authorizationPolicyList.Items = append(authorizationPolicyList.Items, ap)
		}
	}

	return &authorizationPolicyList, nil
}

func (r creator) generateExtAuthAuthorizationPolicies(ctx context.Context, client client.Client, api *gatewayv2alpha1.APIRule, rule gatewayv2alpha1.Rule, notPaths []string) (authorizationPolicyList []*securityv1beta1.AuthorizationPolicy, _ error) {
	for i, authorizer := range rule.ExtAuth.ExternalAuthorizers {
		policy, err := r.generateExtAuthAuthorizationPolicy(ctx, client, api, rule, authorizer, notPaths)
		if err != nil {
			return authorizationPolicyList, err
		}

		err = hashbasedstate.AddLabelsToAuthorizationPolicy(policy, i)
		if err != nil {
			return authorizationPolicyList, err
		}

		authorizationPolicyList = append(authorizationPolicyList, policy)
	}

	return authorizationPolicyList, nil
}

func (r creator) generateAuthorizationPolicyForEmptyAuthorizations(ctx context.Context, client client.Client, api *gatewayv2alpha1.APIRule, rule gatewayv2alpha1.Rule, notPaths []string) (*securityv1beta1.AuthorizationPolicy, error) {
	// In case of NoAuth, it will create an ALLOW AuthorizationPolicy bypassing any other AuthorizationPolicies.
	ap, err := r.generateAuthorizationPolicy(ctx, client, api, rule, &gatewayv2alpha1.JwtAuthorization{}, notPaths)
	if err != nil {
		return nil, err
	}

	// If there is no other authorization, we can safely assume that the index of this authorization in the array
	// in the YAML is 0.
	if err := hashbasedstate.AddLabelsToAuthorizationPolicy(ap, 0); err != nil {
		return nil, err
	}

	return ap, nil
}

func baseAuthorizationPolicyBuilder(apiRule *gatewayv2alpha1.APIRule, rule gatewayv2alpha1.Rule) (*builders.AuthorizationPolicyBuilder, error) {
	namePrefix := fmt.Sprintf("%s-", apiRule.Name)
	namespace, err := gatewayv2alpha1.FindServiceNamespace(apiRule, rule)
	if err != nil {
		return nil, fmt.Errorf("finding service namespace: %w", err)
	}

	return builders.NewAuthorizationPolicyBuilder().
			WithGenerateName(namePrefix).
			WithNamespace(namespace).
			WithLabel(processing.OwnerLabel, fmt.Sprintf("%s.%s", apiRule.Name, apiRule.Namespace)),
		nil
}

func (r creator) generateExtAuthAuthorizationPolicy(ctx context.Context, client client.Client, api *gatewayv2alpha1.APIRule, rule gatewayv2alpha1.Rule, authorizerName string, notPaths []string) (*securityv1beta1.AuthorizationPolicy, error) {
	spec, err := r.generateExtAuthAuthorizationPolicySpec(ctx, client, api, rule, authorizerName, notPaths)
	if err != nil {
		return nil, err
	}

	apBuilder, err := baseAuthorizationPolicyBuilder(api, rule)
	if err != nil {
		return nil, fmt.Errorf("error creating base AuthorizationPolicy builder: %w", err)
	}

	apBuilder.
		WithSpec(builders.NewAuthorizationPolicySpecBuilder().FromAP(spec).Get())

	return apBuilder.Get(), nil
}

func (r creator) generateAuthorizationPolicy(ctx context.Context, client client.Client, apiRule *gatewayv2alpha1.APIRule, rule gatewayv2alpha1.Rule, authorization *gatewayv2alpha1.JwtAuthorization, notPaths []string) (*securityv1beta1.AuthorizationPolicy, error) {
	spec, err := r.generateAuthorizationPolicySpec(ctx, client, apiRule, rule, authorization, notPaths)
	if err != nil {
		return nil, err
	}

	apBuilder, err := baseAuthorizationPolicyBuilder(apiRule, rule)
	if err != nil {
		return nil, fmt.Errorf("error creating base AuthorizationPolicy builder: %w", err)
	}

	apBuilder.WithSpec(
		builders.NewAuthorizationPolicySpecBuilder().
			FromAP(spec).
			Get())

	return apBuilder.Get(), nil
}

func (r creator) generateExtAuthAuthorizationPolicySpec(ctx context.Context, client client.Client, api *gatewayv2alpha1.APIRule, rule gatewayv2alpha1.Rule, providerName string, notPaths []string) (*v1beta1.AuthorizationPolicy, error) {
	podSelector, err := gatewayv2alpha1.GetSelectorFromService(ctx, client, api, rule)
	if err != nil {
		return nil, err
	}

	hosts, err := getHostsFromAPIRule(api, r)
	if err != nil {
		return nil, err
	}

	authorizationPolicySpecBuilder := builders.NewAuthorizationPolicySpecBuilder().WithSelector(podSelector.Selector)
	return authorizationPolicySpecBuilder.
		WithAction(v1beta1.AuthorizationPolicy_CUSTOM).
		WithProvider(providerName).
		WithRule(baseExtAuthRuleBuilder(rule, hosts, notPaths).Get()).
		Get(), nil
}

func (r creator) generateAuthorizationPolicySpec(ctx context.Context, client client.Client, api *gatewayv2alpha1.APIRule, rule gatewayv2alpha1.Rule, authorization *gatewayv2alpha1.JwtAuthorization, notPaths []string) (*v1beta1.AuthorizationPolicy, error) {
	podSelector, err := gatewayv2alpha1.GetSelectorFromService(ctx, client, api, rule)
	if err != nil {
		return nil, err
	}

	authorizationPolicySpecBuilder := builders.NewAuthorizationPolicySpecBuilder().
		WithSelector(podSelector.Selector)

	hosts, err := getHostsFromAPIRule(api, r)
	if err != nil {
		return nil, err
	}

	// If RequiredScopes are configured, we need to generate a separate Rule for each scopeKey in defaultScopeKeys
	if len(authorization.RequiredScopes) > 0 {
		for _, scopeKey := range defaultScopeKeys {
			ruleBuilder := baseRuleBuilder(rule, hosts, r.oryPassthrough, notPaths)
			for _, scope := range authorization.RequiredScopes {
				ruleBuilder.WithWhenCondition(
					builders.NewConditionBuilder().WithKey(scopeKey).WithValues([]string{scope}).Get())
			}

			for _, aud := range authorization.Audiences {
				ruleBuilder.WithWhenCondition(
					builders.NewConditionBuilder().WithKey(audienceKey).WithValues([]string{aud}).Get())
			}

			authorizationPolicySpecBuilder.WithRule(ruleBuilder.Get())
		}
	} else { // Only one AP rule should be generated for other scenarios
		ruleBuilder := baseRuleBuilder(rule, hosts, r.oryPassthrough, notPaths)
		for _, aud := range authorization.Audiences {
			ruleBuilder.WithWhenCondition(
				builders.NewConditionBuilder().WithKey(audienceKey).WithValues([]string{aud}).Get())
		}
		authorizationPolicySpecBuilder.WithRule(ruleBuilder.Get())
	}

	return authorizationPolicySpecBuilder.Get(), nil
}

// getHostsFromAPIRule extracts all FQDNs for which the APIRule should match.
// If the APIRule contains short host names, it will use the domain of the specified gateway to generate FQDNs for them.
// This is done by concatenating the short host name with the wildcard domain of the gateway.
// For example, if the short host name is "foo" and the gateway defines itself to ".*.example.com"
// the resulting FQDN will be "foo.example.com".
// For FQDN host names, it will just return the host name as is.
//
// Returns:
//   - a slice of FQDN host names.
//   - an error if the gateway is not provided and short host names are used.
func getHostsFromAPIRule(api *gatewayv2alpha1.APIRule, r creator) ([]string, error) {
	var hosts []string
	var gatewayDomain string

	if r.gateway != nil {
		for _, server := range r.gateway.Spec.Servers {
			if len(server.Hosts) > 0 {
				gatewayDomain = strings.TrimPrefix(server.Hosts[0], "*.")

				// This break statement here ensures that the host used for the gateway is the first one.
				// Possibly it might be better to return an error if there are multiple different hosts in the same gateway.
				break
			}
		}
	}

	for _, h := range api.Spec.Hosts {
		host := string(*h)
		if !helpers.IsShortHostName(host) {
			hosts = append(hosts, host)
		} else {
			if r.gateway == nil {
				return nil, fmt.Errorf("gateway must be provided when using short host name")
			}

			if gatewayDomain == "" {
				return nil, fmt.Errorf("gateway with host definition must be provided when using short host name")
			}
			hosts = append(hosts, default_domain.GetHostWithDomain(host, gatewayDomain))
		}
	}

	return hosts, nil
}

// standardizeRulePath converts wildcard `/*` path to post Istio 1.22 Envoy template format `/{**}`.
func standardizeRulePath(path string) string {
	if path == "/*" {
		return "/{**}"
	}
	return path
}

func withTo(b *builders.RuleBuilder, hosts []string, rule gatewayv2alpha1.Rule, notPaths []string) *builders.RuleBuilder {
	return b.WithTo(
		builders.NewToBuilder().
			WithOperation(builders.NewOperationBuilder().
				Hosts(hosts...).
				WithMethodsV2alpha1(rule.Methods).
				WithPath(standardizeRulePath(rule.Path)).
				WithNotPaths(notPaths).Get()).
			Get())
}

func withFrom(b *builders.RuleBuilder, rule gatewayv2alpha1.Rule, oryPassthrough bool) *builders.RuleBuilder {
	if rule.Jwt != nil {
		// only viable when migration step is happening. Do not add ingressgateway source during migration
		if oryPassthrough {
			return b.WithFrom(builders.NewFromBuilder().
				WithForcedJWTAuthorizationV2alpha1(rule.Jwt.Authentications).
				Get())
		}

		return b.WithFrom(builders.NewFromBuilder().
			WithForcedJWTAuthorizationV2alpha1(rule.Jwt.Authentications).
			WithIngressGatewaySource().
			Get())
	}

	if rule.ExtAuth != nil && rule.ExtAuth.Restrictions != nil {
		// only viable when migration step is happening. Do not add ingressgateway source during migration
		if oryPassthrough {
			b.WithFrom(builders.NewFromBuilder().
				WithForcedJWTAuthorizationV2alpha1(rule.ExtAuth.Restrictions.Authentications).
				Get())
		}

		return b.WithFrom(builders.NewFromBuilder().
			WithForcedJWTAuthorizationV2alpha1(rule.ExtAuth.Restrictions.Authentications).
			WithIngressGatewaySource().
			Get())
	}

	if oryPassthrough {
		b.WithFrom(builders.NewFromBuilder().
			WithOathkeeperProxySource().
			Get())
	}

	return b.WithFrom(builders.NewFromBuilder().
		WithIngressGatewaySource().
		Get())
}

// baseExtAuthRuleBuilder returns ruleBuilder with To
func baseExtAuthRuleBuilder(rule gatewayv2alpha1.Rule, hosts, notPaths []string) *builders.RuleBuilder {
	builder := builders.NewRuleBuilder()
	builder = withTo(builder, hosts, rule, notPaths)

	return builder
}

// baseRuleBuilder returns ruleBuilder with To and From
func baseRuleBuilder(rule gatewayv2alpha1.Rule, hosts []string, oryPassthrough bool, notPaths []string) *builders.RuleBuilder {
	builder := builders.NewRuleBuilder()
	// If the migration is happening, do not add hosts to the rule, to allow internal traffic during migration step
	if oryPassthrough {
		builder = withTo(builder, nil, rule, notPaths)
	} else {
		builder = withTo(builder, hosts, rule, notPaths)
	}
	builder = withFrom(builder, rule, oryPassthrough)

	return builder
}

func generateNotPaths(rules []gatewayv2alpha1.Rule, currentRule gatewayv2alpha1.Rule) []string {
	var notPaths []string
	beforeCurrentRule := true

	for _, rule := range rules {
		if standardizeRulePath(rule.Path) == "/{**}" || rule.Path == "/" {
			continue
		}
		if rule.Path == currentRule.Path {
			beforeCurrentRule = false
			continue
		}
		if methodsContainsAny(rule.Methods, currentRule.Methods) && beforeCurrentRule {
			notPaths = append(notPaths, rule.Path)
		}
	}

	return notPaths
}

func methodsContainsAny(methods, currentRuleMethods []gatewayv2alpha1.HttpMethod) bool {
	for _, method := range methods {
		if slices.Contains(currentRuleMethods, method) {
			return true
		}
	}

	return false
}
