package diagnose

// Diagnostic codes are MCP Doctor's public API.
//
// Rules for these constants, which must not be broken:
//
//   - A code is never reused for a different meaning.
//   - A code is never renumbered. MDC1009 means the same thing in v1.0.0 as in v2.0.0.
//   - New codes are added, never inserted, so a range is stable forever.
//   - A code is removed only in a major release, and only with a changelog entry.
//
// Ranges:
//
//	MDC0xxx  configuration discovery and file access
//	MDC1xxx  static configuration validation
//	MDC2xxx  live protocol diagnostics
//	MDC3xxx  tool-level problems (bad flags, unwritable output)
//
// The numeric ranges exist for humans reading the list. Matching should always be done
// on the full string.
const (
	// --- Configuration discovery and file access (MDC0xxx) ---

	// CodeConfigNotFound means a config file that was expected does not exist. Usually
	// benign: the user simply has no servers configured for that client yet.
	CodeConfigNotFound = "MDC0001"

	// CodeConfigUnreadable means the file exists but could not be read, usually due to
	// permissions or because it is a directory.
	CodeConfigUnreadable = "MDC0002"

	// CodeConfigTooLarge means the file exceeds the configured size limit and was not
	// parsed. Reading it whole would be a memory-exhaustion vector.
	CodeConfigTooLarge = "MDC0003"

	// CodeConfigNotJSON means the file is not valid JSON.
	CodeConfigNotJSON = "MDC0004"

	// CodeConfigSymlink means the path is a symlink. Informational by default: symlinked
	// configs are legitimate, but the user should know which real file was read.
	CodeConfigSymlink = "MDC0005"

	// CodeConfigJSONType means the file is valid JSON but not an object at the top
	// level, so it cannot be a client configuration.
	CodeConfigJSONType = "MDC0006"

	// CodeUnknownClient means an unknown top-level key was found where a known set was
	// expected. This is how we notice a client changed its config format without
	// breaking loudly for users.
	CodeUnknownClient = "MDC0007"

	// CodeServersNotObject means the mcpServers key exists but is not an object.
	CodeServersNotObject = "MDC0008"

	// CodeServerNotObject means an entry inside mcpServers is not an object.
	CodeServerNotObject = "MDC0009"

	// CodeConfigDuplicateKey means the same JSON key appears more than once in one
	// object. Standard JSON parsers silently take the last one; we report it because
	// almost always indicates a merge accident.
	CodeConfigDuplicateKey = "MDC0010"

	// CodeConfigUnsupportedFormat means the file is a configuration format MCP Doctor
	// detects but does not parse, such as Continue's YAML or Codex's TOML. Reporting it
	// explicitly is honest; pretending to support a format would not be.
	CodeConfigUnsupportedFormat = "MDC0011"

	// --- Static configuration validation (MDC1xxx) ---

	// CodeServerNameInvalid means a server name is empty or contains characters that
	// break every client UI.
	CodeServerNameInvalid = "MDC1001"

	// CodeServerNoTransport means the entry defines neither a command (stdio) nor a url
	// (HTTP). There is nothing to connect to.
	CodeServerNoTransport = "MDC1002"

	// CodeServerBothTransports means the entry defines both command and url. The client
	// has to pick one arbitrarily; the user almost certainly did not intend both.
	CodeServerBothTransports = "MDC1003"

	// CodeCommandEmpty means the command field is present but empty or whitespace.
	CodeCommandEmpty = "MDC1004"

	// CodeCommandNotAbsolute means command is a bare name that will be resolved against
	// PATH. That is legitimate, but it means the server can change meaning depending on
	// the environment, which is worth surfacing.
	CodeCommandNotAbsolute = "MDC1005"

	// CodeCommandNotFound means the command could not be resolved on PATH. This is the
	// single most common real MCP failure.
	CodeCommandNotFound = "MDC1006"

	// CodeCommandNotExecutable means the path exists but is not a regular file or lacks
	// execute permission.
	CodeCommandNotExecutable = "MDC1007"

	// CodeArgsInvalid means the args field is not an array of strings.
	CodeArgsInvalid = "MDC1008"

	// CodeArgEmpty means an argument is an empty string. Rarely intended and a common
	// result of a bad shell-quoting copy-paste.
	CodeArgEmpty = "MDC1009"

	// CodeEnvInvalid means the env field is not an object of strings.
	CodeEnvInvalid = "MDC1010"

	// CodeEnvNameInvalid means an environment variable name is not a valid identifier.
	// Most clients silently drop these, so the server starts without the variable it
	// needed.
	CodeEnvNameInvalid = "MDC1011"

	// CodeCwdInvalid means the cwd field is not a string, or names something that is not
	// an existing directory.
	CodeCwdInvalid = "MDC1012"

	// CodeURLInvalid means the url field is not a valid absolute URL.
	CodeURLInvalid = "MDC1013"

	// CodeURLSchemeUnsupported means the URL scheme is not http or https.
	CodeURLSchemeUnsupported = "MDC1014"

	// CodeURLInsecure means a non-loopback host is addressed over plain http, sending
	// credentials and tool traffic in cleartext.
	CodeURLInsecure = "MDC1015"

	// CodeURLCredentials means the URL embeds a username or password.
	CodeURLCredentials = "MDC1016"

	// CodeTypeMismatch means a known transport type field disagrees with the fields
	// actually present.
	CodeTypeMismatch = "MDC1017"

	// CodeUnknownField means the server entry contains a key MCP Doctor does not
	// recognise. Almost always benign, and reported so that client format drift is
	// visible.
	CodeUnknownField = "MDC1018"

	// CodeServerDisabled means the server is marked disabled and will not be started by
	// the client.
	CodeServerDisabled = "MDC1019"

	// CodeSecretInConfig means a value looks like a credential. Reported so users notice
	// secrets committed to a repository, never with the value itself.
	CodeSecretInConfig = "MDC1020"

	// CodeEnvInterpolation means an env value contains an expansion placeholder such as
	// ${VAR}, which most clients do not expand and will pass through literally.
	CodeEnvInterpolation = "MDC1021"

	// CodeArgsShellMetacharacter means an argument contains shell metacharacters. Harmless
	// when passed as an argv element, but a strong signal the user copied a shell
	// command into the args array and it will not behave as written.
	CodeArgsShellMetacharacter = "MDC1022"

	// CodeCommandContainsSeparator means the command string contains a path separator
	// or shell metacharacter, suggesting a command line was pasted where an argv was
	// expected.
	CodeCommandContainsSeparator = "MDC1023"

	// CodeHeadersInvalid means the headers field is not an object of strings.
	CodeHeadersInvalid = "MDC1024"

	// CodeTimeoutInvalid means the timeout field is not a positive number of seconds.
	CodeTimeoutInvalid = "MDC1025"

	// CodeServerNameShadowed means two servers with the same name appear in one config.
	CodeServerNameShadowed = "MDC1026"

	// --- Live protocol diagnostics (MDC2xxx) ---

	// CodeProbeNotAuthorized means a protocol check was requested without the explicit
	// authorisation flag. This is a tool-level refusal, not a server problem.
	CodeProbeNotAuthorized = "MDC2000"

	// CodeSpawnFailed means the server process could not be started at all: the binary
	// is missing, is not executable, or the working directory does not exist.
	CodeSpawnFailed = "MDC2001"

	// CodeExitedBeforeInit means the server process exited before completing
	// initialization. Its stderr is attached, because that is where the reason is.
	CodeExitedBeforeInit = "MDC2002"

	// CodeStartupTimeout means the server did not respond to initialize within the
	// configured timeout.
	CodeStartupTimeout = "MDC2003"

	// CodeMalformedMessage means the server wrote something to stdout that is not a
	// valid JSON-RPC message.
	CodeMalformedMessage = "MDC2004"

	// CodeMessageTooLarge means a message exceeded the read limit and was refused. This
	// is a resource-protection control, reported rather than silently tolerated.
	CodeMessageTooLarge = "MDC2005"

	// CodeVersionMismatch means the server negotiated a protocol version MCP Doctor
	// does not know, or negotiated one outside the supported set.
	CodeVersionMismatch = "MDC2006"

	// CodeCapabilityMismatch means the server advertised a capability that its
	// subsequent responses did not honour, such as advertising tools then failing
	// tools/list.
	CodeCapabilityMismatch = "MDC2007"

	// CodeToolsListFailed means tools/list failed despite tools being advertised.
	CodeToolsListFailed = "MDC2008"

	// CodeToolSchemaInvalid means a tool advertised an inputSchema that is not a valid
	// JSON Schema object. Clients will reject or mis-handle such a tool at call time.
	CodeToolSchemaInvalid = "MDC2009"

	// CodeTransportFailed means the HTTP transport could not complete the request:
	// connection refused, TLS failure, or an unexpected status.
	CodeTransportFailed = "MDC2010"

	// CodeHTTPStatus means the endpoint answered with a non-success HTTP status.
	CodeHTTPStatus = "MDC2011"

	// CodeServerWroteToStdoutNonProtocol means the server wrote non-protocol text to
	// stdout. The spec forbids this, and it will corrupt any client that is stricter
	// than we are.
	CodeServerWroteToStdoutNonProtocol = "MDC2012"

	// CodeRemoteProbeBlocked means a non-loopback URL was configured and probing it
	// requires an extra opt-in flag.
	CodeRemoteProbeBlocked = "MDC2013"

	// CodeRemoteNotAllowed means --allow-remote was needed but not supplied.
	CodeRemoteNotAllowed = "MDC2014"

	// CodeShutdownUnclean means the server did not exit after stdin was closed and
	// SIGTERM was sent, and had to be killed. Often indicates it held the pipe open.
	CodeShutdownUnclean = "MDC2015"

	// CodeServerStderr means the server logged something to stderr that looks like an
	// error. Informational, because most servers log warnings to stderr routinely.
	CodeServerStderr = "MDC2016"

	// CodeEmptyToolName means a tool was advertised with an empty or non-string name.
	CodeEmptyToolName = "MDC2017"

	// CodeDuplicateToolName means two tools share a name, so calls are ambiguous.
	CodeDuplicateToolName = "MDC2018"

	// CodeToolCountExceedsLimit means tools/list returned more tools than the configured
	// limit. Treated as a resource concern and reported.
	CodeToolCountExceedsLimit = "MDC2019"
)

// configCodes lists the MDC0xxx codes: configuration discovery and file access.
func configCodes() []string {
	return []string{
		CodeConfigNotFound,
		CodeConfigUnreadable,
		CodeConfigTooLarge,
		CodeConfigNotJSON,
		CodeConfigSymlink,
		CodeConfigJSONType,
		CodeUnknownClient,
		CodeServersNotObject,
		CodeServerNotObject,
		CodeConfigDuplicateKey,
		CodeConfigUnsupportedFormat,
	}
}

// serverCodes lists the MDC1xxx codes: static configuration validation.
func serverCodes() []string {
	return []string{
		CodeServerNameInvalid,
		CodeServerNoTransport,
		CodeServerBothTransports,
		CodeCommandEmpty,
		CodeCommandNotAbsolute,
		CodeCommandNotFound,
		CodeCommandNotExecutable,
		CodeArgsInvalid,
		CodeArgEmpty,
		CodeEnvInvalid,
		CodeEnvNameInvalid,
		CodeCwdInvalid,
		CodeURLInvalid,
		CodeURLSchemeUnsupported,
		CodeURLInsecure,
		CodeURLCredentials,
		CodeTypeMismatch,
		CodeUnknownField,
		CodeServerDisabled,
		CodeSecretInConfig,
		CodeEnvInterpolation,
		CodeArgsShellMetacharacter,
		CodeCommandContainsSeparator,
		CodeHeadersInvalid,
		CodeTimeoutInvalid,
		CodeServerNameShadowed,
	}
}

// transportCodes lists the MDC2xxx codes: live protocol diagnostics.
func transportCodes() []string {
	return []string{
		CodeProbeNotAuthorized,
		CodeSpawnFailed,
		CodeExitedBeforeInit,
		CodeStartupTimeout,
		CodeMalformedMessage,
		CodeMessageTooLarge,
		CodeVersionMismatch,
		CodeCapabilityMismatch,
		CodeToolsListFailed,
		CodeToolSchemaInvalid,
		CodeTransportFailed,
		CodeHTTPStatus,
		CodeServerWroteToStdoutNonProtocol,
		CodeRemoteProbeBlocked,
		CodeRemoteNotAllowed,
		CodeShutdownUnclean,
		CodeServerStderr,
		CodeEmptyToolName,
		CodeDuplicateToolName,
		CodeToolCountExceedsLimit,
	}
}
