import * as DialogPrimitive from "@radix-ui/react-dialog";
import { useLocation, useRouter } from "@tanstack/react-router";
import { Command } from "cmdk";
import { ArrowUpRight, CornerDownLeft, Search } from "lucide-react";
import { useCallback, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { useNavItems } from "@/components/navigation/navItems";
import { IS_ENTERPRISE } from "@/lib/constants/config";
import { cn } from "@/lib/utils";

import { findCurrentCommandId, flattenNavItems, modKeyLabel, type PaletteNavCommand, searchCommands } from "./commandPalette.utils";

interface CommandGroupView {
	id: string;
	heading: string;
	commands: PaletteNavCommand[];
}

export default function CommandPalette({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
	const { t } = useTranslation("shell");
	const router = useRouter();
	const pathname = useLocation({ select: (l) => l.pathname });
	const search = useLocation({ select: (l) => l.searchStr ?? "" });
	const navItems = useNavItems();
	const [query, setQuery] = useState("");
	const [selected, setSelected] = useState("");
	const [wasOpen, setWasOpen] = useState(open);

	// Reset while rendering the opening frame, not in an effect after it, so the first frame
	// already shows an empty query instead of painting the last one and re-rendering.
	if (open !== wasOpen) {
		setWasOpen(open);
		if (open) {
			setQuery("");
			setSelected("");
		}
	}

	const commands = useMemo(() => flattenNavItems(navItems, IS_ENTERPRISE), [navItems]);
	const currentId = useMemo(() => findCurrentCommandId(commands, pathname, search), [commands, pathname, search]);

	const groups = useMemo<CommandGroupView[]>(() => {
		if (query.trim()) {
			const results = searchCommands(commands, query);
			// No group at all on a miss, or its heading renders above the empty state.
			return results.length ? [{ id: "results", heading: t("commandPalette.groups.pages"), commands: results }] : [];
		}
		const sections = new Map<string, CommandGroupView>();
		for (const c of commands) {
			const key = c.section ?? "";
			if (!sections.has(key))
				sections.set(key, { id: `section:${key}`, heading: c.section ?? t("commandPalette.groups.general"), commands: [] });
			sections.get(key)!.commands.push(c);
		}
		return [...sections.values()];
	}, [commands, query, t]);

	const commandByValue = useMemo(() => {
		const map = new Map<string, PaletteNavCommand>();
		for (const g of groups) for (const c of g.commands) map.set(c.id, c);
		return map;
	}, [groups]);
	// Filtering is ours (cmdk's scorer ranks breadcrumb hits as high as title hits), so cmdk never
	// re-selects on its own: a selection that left the result list falls back to the top result.
	const activeValue = commandByValue.has(selected) ? selected : (commandByValue.keys().next().value ?? "");

	const run = useCallback(
		(command: PaletteNavCommand, inNewTab = false) => {
			if (inNewTab || command.isExternal) {
				window.open(command.href, "_blank", "noopener,noreferrer");
				if (command.isExternal) onOpenChange(false);
				return;
			}
			onOpenChange(false);
			router.history.push(command.href);
		},
		[onOpenChange, router],
	);

	const onKeyDown = (event: React.KeyboardEvent) => {
		if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
			const command = commandByValue.get(activeValue);
			if (!command) return;
			event.preventDefault();
			run(command, true);
		}
	};

	const mod = modKeyLabel();

	return (
		<DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
			<DialogPrimitive.Portal>
				<DialogPrimitive.Overlay
					data-testid="command-palette-overlay"
					className="data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 fixed inset-0 z-50 bg-black/25 duration-100 dark:bg-black/50"
				/>
				<DialogPrimitive.Content
					data-testid="command-palette"
					aria-describedby={undefined}
					onEscapeKeyDown={(e) => {
						// First Escape clears the query, the second closes.
						if (query) {
							e.preventDefault();
							setQuery("");
						}
					}}
					className={cn(
						"dark:bg-card fixed top-[min(20vh,160px)] left-1/2 z-50 w-[calc(100%-2rem)] max-w-[640px] -translate-x-1/2 overflow-hidden rounded-xl border border-gray-200 bg-white shadow-[0_24px_64px_-12px_rgba(0,0,0,0.25)] outline-none dark:border-zinc-800",
						"data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[state=closed]:zoom-out-[0.98] data-[state=open]:zoom-in-[0.98] duration-100 ease-out",
					)}
				>
					<DialogPrimitive.Title className="sr-only">{t("commandPalette.title")}</DialogPrimitive.Title>
					<Command
						label={t("commandPalette.title")}
						shouldFilter={false}
						loop
						value={activeValue}
						onValueChange={setSelected}
						onKeyDown={onKeyDown}
					>
						<div className="flex h-14 items-center gap-3 border-b border-gray-100 px-4 dark:border-zinc-800">
							<Search className="text-muted-foreground size-4 shrink-0" />
							<Command.Input
								data-testid="command-palette-input"
								value={query}
								onValueChange={(q) => {
									setQuery(q);
									setSelected("");
								}}
								placeholder={t("commandPalette.placeholder")}
								className="placeholder:text-muted-foreground text-foreground h-full flex-1 bg-transparent text-[15px] outline-none"
							/>
							{/* i18n-ignore */}
							<Kbd>esc</Kbd>
						</div>
						<Command.List
							data-testid="command-palette-list"
							className="custom-scrollbar h-(--cmdk-list-height) max-h-[min(440px,55vh)] scroll-py-2 overflow-y-auto overscroll-contain px-2 transition-[height] duration-150 ease-out"
						>
							<div className="py-2">
								<Command.Empty className="text-muted-foreground py-10 text-center text-sm">
									{t("commandPalette.empty", { query: query.trim() })}
								</Command.Empty>
								{groups.map((group) => (
									<Command.Group
										key={group.id}
										heading={group.heading}
										className="[&_[cmdk-group-heading]]:text-muted-foreground [&_[cmdk-group-heading]]:px-2.5 [&_[cmdk-group-heading]]:pt-1 [&_[cmdk-group-heading]]:pb-1.5 [&_[cmdk-group-heading]]:text-[11px] [&_[cmdk-group-heading]]:font-medium [&:not(:first-child)]:mt-2"
									>
										{group.commands.map((command) => (
											<PaletteItem
												key={command.id}
												value={command.id}
												command={command}
												showSection={group.id === "results"}
												isCurrent={command.id === currentId}
												currentLabel={t("commandPalette.current")}
												onSelect={() => run(command)}
											/>
										))}
									</Command.Group>
								))}
							</div>
						</Command.List>
						<div className="text-muted-foreground flex h-10 items-center gap-4 border-t border-gray-100 px-4 text-[11px] dark:border-zinc-800">
							<span className="flex items-center gap-1.5">
								<Kbd>↑</Kbd>
								<Kbd>↓</Kbd>
								{t("commandPalette.footer.navigate")}
							</span>
							<span className="flex items-center gap-1.5">
								<Kbd>↵</Kbd>
								{t("commandPalette.footer.open")}
							</span>
							<span className="flex items-center gap-1.5">
								<Kbd>{mod}</Kbd>
								<Kbd>↵</Kbd>
								{t("commandPalette.footer.newTab")}
							</span>
						</div>
					</Command>
				</DialogPrimitive.Content>
			</DialogPrimitive.Portal>
		</DialogPrimitive.Root>
	);
}

function PaletteItem({
	value,
	command,
	showSection,
	isCurrent,
	currentLabel,
	onSelect,
}: {
	value: string;
	command: PaletteNavCommand;
	showSection: boolean;
	isCurrent: boolean;
	currentLabel: string;
	onSelect: () => void;
}) {
	const Icon = command.icon;
	return (
		<Command.Item
			value={value}
			onSelect={onSelect}
			data-testid={`command-palette-item-${command.id}`}
			className="group text-foreground/85 data-[selected=true]:text-foreground relative flex h-10 cursor-pointer items-center gap-3 rounded-lg px-2.5 text-sm transition-colors duration-75 select-none data-[selected=true]:bg-gray-100 dark:data-[selected=true]:bg-zinc-800/80"
		>
			<Icon className="text-muted-foreground group-data-[selected=true]:text-foreground size-4 shrink-0 transition-colors" />
			<span className="min-w-0 truncate">{command.title}</span>
			{showSection && command.section && <span className="text-muted-foreground min-w-0 shrink truncate text-xs">{command.section}</span>}
			<span className="ml-auto flex shrink-0 items-center gap-2">
				{isCurrent && (
					<span className="text-muted-foreground rounded-sm border border-gray-200 px-1.5 py-px text-[10px] leading-4 dark:border-zinc-700">
						{currentLabel}
					</span>
				)}
				{command.isExternal ? (
					<ArrowUpRight className="text-muted-foreground size-3.5" />
				) : (
					<CornerDownLeft className="text-muted-foreground size-3.5 opacity-0 transition-opacity duration-75 group-data-[selected=true]:opacity-100" />
				)}
			</span>
		</Command.Item>
	);
}

function Kbd({ children }: { children: React.ReactNode }) {
	return (
		<kbd className="text-muted-foreground inline-flex h-5 min-w-5 items-center justify-center rounded border border-gray-200 bg-gray-50 px-1 font-sans text-[10px] font-medium dark:border-zinc-700 dark:bg-zinc-900">
			{children}
		</kbd>
	);
}