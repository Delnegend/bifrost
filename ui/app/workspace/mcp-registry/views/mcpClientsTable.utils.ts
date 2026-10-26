import { MCPClient } from "@/lib/types/mcp";
import { perUserDiscoveryOn } from "@/lib/utils/mcpPerUserDiscovery";

/**
 * exchangeVerifyCopy is the text of the token_exchange "Verify as me" dialog
 * for client: lead is followed by the client's name. A server that discovers
 * tools per caller keeps nothing from the exchange, so its text speaks of a
 * verification only, never of a discovery credential.
 */
export function exchangeVerifyCopy(client: { state: MCPClient["state"]; config: Pick<MCPClient["config"], "per_user_discovery"> } | null): {
	title: string;
	description: string;
	lead: string;
	detail: string;
} {
	const pending = client?.state === "pending_verification";
	const title = pending ? "Verify as me" : "Re-verify as me";
	if (perUserDiscoveryOn(client?.config)) {
		return {
			title,
			description: "Verify this server using your identity. Nothing is kept.",
			lead: "This exchanges your own signed-in identity to verify",
			detail:
				"This server discovers tools per caller, so the exchanged credential only verifies it and is not kept. Each caller's token is exchanged automatically on every request.",
		};
	}
	let detail =
		"That credential is only used to periodically fetch this server's tool list, not for real user requests, whose tokens are exchanged automatically on every request.";
	if (!pending) {
		detail += " You only need this if the credential badge shows it's expired, but running it any time is safe.";
	}
	return {
		title,
		description: pending
			? "Establish Bifrost's discovery credential using your identity."
			: "Renew Bifrost's own discovery credential using your identity.",
		lead: `This exchanges your own signed-in identity to ${pending ? "establish" : "renew"} Bifrost's discovery credential for`,
		detail,
	};
}

/**
 * canReconnectMCPClient reports whether the "Reconnect" action applies to
 * this client at all — mirrors core/mcp/credstore's RequiresPerCallConnection
 * exactly, so the UI never offers an action the backend rejects with
 * ErrMCPReconnectNotApplicable:
 *
 *   - per_user_oauth, per_user_headers, token_exchange never hold a shared
 *     upstream connection — auth is resolved per request/user identity.
 *   - A shared oauth/headers/none client on an http connection with
 *     needs_session_stickiness nil/false (the default for newly created
 *     clients) is also per-call: it dials fresh on every call, so there is
 *     no persistent connection to reconnect either.
 *   - Every other combination (any non-http connection type, or an http
 *     shared client with needs_session_stickiness === true) holds a real
 *     persistent connection Reconnect can act on.
 */
export function canReconnectMCPClient(
	config: Pick<MCPClient["config"], "auth_type" | "connection_type" | "needs_session_stickiness">,
): boolean {
	const alwaysPerCall =
		config.auth_type === "per_user_oauth" || config.auth_type === "per_user_headers" || config.auth_type === "token_exchange";
	const sharedButPerCallViaStickiness = config.connection_type === "http" && config.needs_session_stickiness !== true;
	return !(alwaysPerCall || sharedButPerCallViaStickiness);
}