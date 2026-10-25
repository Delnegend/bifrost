import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { cn } from "@/lib/utils";
import { TriangleAlert } from "lucide-react";

// ToolsPendingNotice tells a caller who just signed in to an MCP client that its
// tools could not be loaded with the new credential. The credential is stored,
// so connecting to the client again from their MCP client retries.
export function ToolsPendingNotice({ clientName, error, className }: { clientName: string; error?: string; className?: string }) {
	return (
		<Alert variant="warning" className={cn("text-left", className)} data-testid="mcp-sessions-tools-pending-alert">
			<TriangleAlert />
			<AlertTitle>Tools not loaded yet</AlertTitle>
			<AlertDescription>
				<p>
					Your credential is saved, but Bifrost couldn't load the tools of {clientName}. Ask your MCP client to connect to {clientName}{" "}
					again to retry.
				</p>
				{error && (
					<p className="font-mono text-xs break-all" data-testid="mcp-sessions-tools-pending-error">
						{error}
					</p>
				)}
			</AlertDescription>
		</Alert>
	);
}