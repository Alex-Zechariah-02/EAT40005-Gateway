// Kong vendor facts: the decK members the reader knows, defaults, credential names and refusal codes.

package kong

import "slices"

// Format names the decK file format in the exchange ledger.
const Format = "kong-deck"

// Version is the decK format version read and written (Kong OSS 3.9.x).
const Version = "3.0"

// Refusal codes written at the start of a not-expressible item's note.
const (
	CodeTransform   = "PATH_TRANSFORM_UNSUPPORTED"
	CodeMatch       = "ROUTE_MATCH_UNSUPPORTED"
	CodeAuth        = "AUTH_SCHEME_UNSUPPORTED"
	CodeDeny        = "DENY_LIST_UNSUPPORTED"
	CodeRate        = "RATE_UNSUPPORTED"
	CodeConsumer    = "PER_CONSUMER_UNSUPPORTED"
	CodeUnknown     = "UNKNOWN_FIELD"
	CodeVersion     = "FORMAT_VERSION_UNSUPPORTED"
	CodeCollision   = "TARGET_COLLISION"
	CodeOutOfScope  = "NOT_API_CONFIGURATION"
	CodeNotImported = "ROUTE_NOT_IMPORTED"
)

// Members of the decK root, services, routes, plugins and consumers, from
// Kong's published decK file schema (go-database-reconciler,
// kong_json_schema.json, 13 Oct 2026); anything else is unknown.
var (
	// rootScope are the root members holding API configuration.
	rootScope = []string{"_format_version", "_info", "_transform", "_workspace", "_konnect", "services", "routes", "plugins", "consumers"}
	// rootOther are root lists outside the API configuration.
	rootOther = []string{"_plugin_configs", "ai_models", "ca_certificates", "certificates", "cloned_plugins", "consumer_groups",
		"custom_entities", "custom_plugins", "filter_chains", "key_sets", "keys", "licenses", "partials", "rbac_roles",
		"service_packages", "upstreams", "vaults"}
	// serviceAddress members make up the upstream address.
	serviceAddress = []string{"url", "protocol", "host", "port", "path"}
	// serviceFolded members are read as part of the service itself.
	serviceFolded = []string{"name", "id", "tags", "routes", "plugins", "enabled", "created_at", "updated_at"}
	// serviceOther members are kept for the way back only.
	serviceOther = []string{"retries", "connect_timeout", "write_timeout", "read_timeout", "client_certificate", "tls_verify",
		"tls_verify_depth", "ca_certificates", "tls_sans", "filter_chains"}
	// routeFolded members are read into the route's operations.
	routeFolded = []string{"name", "id", "tags", "paths", "methods", "strip_path", "protocols", "service", "plugins",
		"created_at", "updated_at"}
	// routeMatch members select requests by other than path and method.
	routeMatch = []string{"hosts", "headers", "snis", "sources", "destinations", "expression", "priority"}
	// routeOther members are kept for the way back only.
	routeOther = []string{"preserve_host", "regex_priority", "path_handling", "https_redirect_status_code",
		"request_buffering", "response_buffering", "filter_chains"}
	// pluginMembers are the plugin entity members a mapped plugin may have.
	pluginMembers = []string{"name", "config", "enabled", "protocols", "tags", "id", "instance_name", "service", "route",
		"consumer", "consumer_group", "created_at", "updated_at"}
	// consumerMembers are the consumer members; credentials are never read.
	consumerMembers = []string{"username", "custom_id", "id", "tags", "acls", "groups", "plugins", "created_at",
		"jwt_secrets", "keyauth_credentials", "basicauth_credentials", "hmacauth_credentials", "oauth2_credentials",
		"mtls_auth_credentials"}
	// credentialLists are consumer members holding credentials: counted, never copied.
	credentialLists = []string{"jwt_secrets", "keyauth_credentials", "basicauth_credentials", "hmacauth_credentials",
		"oauth2_credentials", "mtls_auth_credentials"}
)

// Plugins the reader maps; their config members (Kong OSS 3.9 plugin
// schemas). An unknown config member refuses the plugin.
var mapped = map[string][]string{
	"jwt": {"uri_param_names", "cookie_names", "header_names", "claims_to_verify", "key_claim_name", "secret_is_base64",
		"anonymous", "run_on_preflight", "maximum_expiration", "realm"},
	"acl": {"allow", "deny", "allow_when", "deny_when", "hide_groups_header", "include_consumer_groups",
		"always_use_authenticated_groups"},
	"rate-limiting": {"second", "minute", "hour", "day", "month", "year", "limit_by", "header_name", "path", "policy",
		"fault_tolerant", "hide_client_headers", "redis", "redis_host", "redis_port", "redis_password", "redis_username",
		"redis_ssl", "redis_ssl_verify", "redis_server_name", "redis_timeout", "redis_database", "error_code",
		"error_message", "sync_rate"},
	"request-size-limiting": {"allowed_payload_size", "size_unit", "require_content_length"},
}

// unsupportedAuth are sign-in plugins the gateway cannot express; a route
// using one is refused, never imported as public.
var unsupportedAuth = []string{"key-auth", "key-auth-enc", "basic-auth", "hmac-auth", "ldap-auth", "ldap-auth-advanced",
	"oauth2", "oauth2-introspection", "openid-connect", "mtls-auth", "jwt-signer", "vault-auth"}

// periods are the rate-limiting window members in seconds, shortest first.
var periods = []struct {
	name    string
	seconds int64
}{{"second", 1}, {"minute", 60}, {"hour", 3600}, {"day", 86400}, {"month", 0}, {"year", 0}}

// secretWords mark plugin config members holding credentials; such members
// are removed from carried values. Kong marks them "referenceable".
var secretWords = []string{"password", "passphrase", "secret", "private_key", "api_key", "apikey", "token"}

// methods are the HTTP methods an operation may have, in OpenAPI order.
var methods = []string{"GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH"}

// bodyMethods are the methods given a JSON body rule.
var bodyMethods = []string{"PUT", "POST", "PATCH"}

// defaultBodyBytes is the body limit when no request-size-limiting applies.
const defaultBodyBytes = 16384

// maxBodyBytes is the gateway's largest body limit.
const maxBodyBytes = 1 << 20

// sizeUnits are request-size-limiting's units in bytes.
var sizeUnits = map[string]int64{"bytes": 1, "kilobytes": 1 << 10, "megabytes": 1 << 20}

func known(list []string, name string) bool { return slices.Contains(list, name) }
