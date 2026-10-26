import { describe, expect, it } from "vitest";
import { canReconnectMCPClient, exchangeVerifyCopy } from "./mcpClientsTable.utils";

describe("exchangeVerifyCopy", () => {
	it("describes a verification that keeps nothing, for a server that discovers tools per caller", () => {
		for (const state of ["pending_verification", "healthy"] as const) {
			const copy = exchangeVerifyCopy({ state, config: { per_user_discovery: true } });
			expect(copy.description, state).not.toContain("discovery credential");
			expect(copy.lead, state).toBe("This exchanges your own signed-in identity to verify");
			expect(copy.detail, state).toContain("only verifies it and is not kept");
		}
	});

	it("describes the discovery credential Bifrost keeps, for any other server", () => {
		const pending = exchangeVerifyCopy({ state: "pending_verification", config: {} });
		expect(pending.title).toBe("Verify as me");
		expect(pending.description).toBe("Establish Bifrost's discovery credential using your identity.");
		expect(pending.lead).toBe("This exchanges your own signed-in identity to establish Bifrost's discovery credential for");
		expect(pending.detail).not.toContain("badge");

		const verified = exchangeVerifyCopy({ state: "healthy", config: { per_user_discovery: false } });
		expect(verified.title).toBe("Re-verify as me");
		expect(verified.description).toBe("Renew Bifrost's own discovery credential using your identity.");
		expect(verified.detail).toContain("the credential badge shows it's expired");
	});
});

describe("canReconnectMCPClient", () => {
	it("is false for per_user_oauth (always per-call)", () => {
		expect(canReconnectMCPClient({ auth_type: "per_user_oauth", connection_type: "http", needs_session_stickiness: true })).toBe(false);
	});

	it("is false for per_user_headers (always per-call)", () => {
		expect(canReconnectMCPClient({ auth_type: "per_user_headers", connection_type: "http" })).toBe(false);
	});

	it("is false for token_exchange (always per-call)", () => {
		expect(canReconnectMCPClient({ auth_type: "token_exchange", connection_type: "http" })).toBe(false);
	});

	it("is false for a shared oauth http client with needs_session_stickiness omitted (default per-call)", () => {
		expect(canReconnectMCPClient({ auth_type: "oauth", connection_type: "http" })).toBe(false);
	});

	it("is false for a shared oauth http client with needs_session_stickiness explicitly false", () => {
		expect(canReconnectMCPClient({ auth_type: "oauth", connection_type: "http", needs_session_stickiness: false })).toBe(false);
	});

	it("is false for a shared headers http client with needs_session_stickiness omitted", () => {
		expect(canReconnectMCPClient({ auth_type: "headers", connection_type: "http" })).toBe(false);
	});

	it("is true for a shared oauth http client with needs_session_stickiness explicitly true", () => {
		expect(canReconnectMCPClient({ auth_type: "oauth", connection_type: "http", needs_session_stickiness: true })).toBe(true);
	});

	it("is true for a shared headers http client with needs_session_stickiness explicitly true", () => {
		expect(canReconnectMCPClient({ auth_type: "headers", connection_type: "http", needs_session_stickiness: true })).toBe(true);
	});

	it("is true for a shared oauth SSE client regardless of needs_session_stickiness (non-http is always sticky)", () => {
		expect(canReconnectMCPClient({ auth_type: "oauth", connection_type: "sse", needs_session_stickiness: false })).toBe(true);
	});

	it("is true for a shared headers STDIO client regardless of needs_session_stickiness (non-http is always sticky)", () => {
		expect(canReconnectMCPClient({ auth_type: "headers", connection_type: "stdio" })).toBe(true);
	});

	it("is true for auth_type none on a non-http connection", () => {
		expect(canReconnectMCPClient({ auth_type: "none", connection_type: "stdio" })).toBe(true);
	});

	it("is false for auth_type none on http with stickiness off", () => {
		expect(canReconnectMCPClient({ auth_type: "none", connection_type: "http" })).toBe(false);
	});
});