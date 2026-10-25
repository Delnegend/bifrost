// Kept free of store and component imports so pure helpers and their tests can use it.

// One entry in the workspace navigation, shared by the sidebar and the command palette.
export interface NavItem {
	id: string;
	title: string;
	url: string;
	icon: React.ComponentType<{ className?: string }>;
	description?: string;
	isAllowed?: boolean;
	hasAccess: boolean;
	subItems?: NavItem[];
	tag?: string;
	new?: boolean;
	isExternal?: boolean;
	queryParam?: string; // Optional: for tab-based subitems (e.g., "client-settings")
	testId?: string; // Optional: pin the data-testid slug across a rename (else derived from title)
	enterpriseOnly?: boolean; // OSS renders an enterprise upsell here; the command palette hides it outside enterprise
}

export const getNavItemHref = (item: Pick<NavItem, "url" | "queryParam">) => {
	return item.queryParam ? `${item.url}?tab=${item.queryParam}` : item.url;
};