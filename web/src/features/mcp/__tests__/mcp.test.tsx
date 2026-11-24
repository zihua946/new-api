/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useAuthStore } from "@/stores/auth-store";
import { useSystemConfigStore } from "@/stores/system-config-store";

import { MCPManagement } from "..";
import { InvocationList } from "../invocations";
import { InstallationConnection } from "../installation-connection";
import { MCPMarketplace } from "../marketplace";
import { ServerForm } from "../server-form";
import { ServicePage } from "../service-page";
import { TestPanel } from "../test-panel";
import { ToolPanel } from "../tool-panel";
import {
  testPending,
  testRequest,
  type MCPServer,
  type MCPTool,
} from "../types";

const request = vi.hoisted(() => vi.fn());
const streamRequest = vi.hoisted(() => vi.fn());
vi.mock("../test-stream", () => ({ streamMCPTest: streamRequest }));
vi.mock("../api", () => ({ mcpRequest: request }));
vi.mock("@/lib/api", () => ({
  getStatus: vi.fn().mockResolvedValue({ mcp_enabled: true, quota_display_type: "USD" }),
  api: { get: vi.fn(), post: vi.fn(), request: vi.fn() },
}));
const initialSystemConfig = useSystemConfigStore.getState().config;
const clients: QueryClient[] = [];
afterEach(async () => {
  cleanup();
  await Promise.all(clients.map((client) => client.cancelQueries()));
  for (const client of clients) client.clear();
  clients.length = 0;
  request.mockReset();
  streamRequest.mockReset();
  sessionStorage.clear();
  window.history.replaceState({}, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  useAuthStore.getState().auth.reset();
  useSystemConfigStore.getState().setConfig(initialSystemConfig);
});

function renderMCPTest(authMode = "engine_api_key") {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  clients.push(client);
  const view = render(
    <QueryClientProvider client={client}>
      <TestPanel
        server={
          { id: "server-1", kind: "deepseek", auth_mode: authMode } as MCPServer
        }
      />
    </QueryClientProvider>,
  );
  return { ...view, client };
}

describe("MCP management page", () => {
  it("omits authentication from the list and authorization and connection testing from management", async () => {
    const user = userEvent.setup();
    useAuthStore.getState().auth.setUser({ id: 1, username: "root", role: 100 });
    const server = {
      id: "remote-server",
      name: "Search service",
      description: "Search public pages",
      enabled: true,
      auth_mode: "oauth2",
      connection_health: { connected: false, error_code: "REAUTHORIZATION_REQUIRED" },
      connection: {
        transport: "streamable_http",
        url: "https://provider.test/mcp",
      },
    };
    request.mockImplementation(async (path: string) =>
      path.startsWith("/servers?")
        ? { items: [server], page: 1, page_size: 20, total: 1 }
        : server,
    );
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    clients.push(client);
    client.setQueryData(["status"], { mcp_enabled: true });
    render(
      <QueryClientProvider client={client}>
        <MCPManagement />
      </QueryClientProvider>,
    );
    await screen.findByText("Search service");
    expect(screen.queryByText("Authentication")).not.toBeInTheDocument();
    expect(screen.queryByText("OAuth2")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Manage" }));
    const detail = within(await screen.findByRole("dialog"));
    expect(
      await detail.findByRole("button", { name: "Edit configuration" }),
    ).toBeVisible();
    expect(
      detail.queryByRole("button", { name: "Authorize" }),
    ).not.toBeInTheDocument();
    expect(
      detail.queryByRole("button", { name: "Test connection" }),
    ).not.toBeInTheDocument();
    expect(detail.getByText("https://provider.test/mcp")).toBeVisible();
    expect(detail.queryByText("Failed")).not.toBeInTheDocument();
  });

  it("shows empty services and invocation history after MCP is enabled", async () => {
    const user = userEvent.setup();
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: "root", role: 100 });
    request.mockImplementation(async (path: string) => ({ items: path.startsWith("/usage") ? null : [], page: 1, page_size: 20, total: 0 }));
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    clients.push(client);
    client.setQueryData(["status"], { mcp_enabled: true });
    render(
      <QueryClientProvider client={client}>
        <MCPManagement />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("No MCP servers found")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Create MCP server" }),
    ).toBeEnabled();
    await user.click(screen.getByRole("tab", { name: "Invocations" }));
    expect(await screen.findByText("No invocations found")).toBeInTheDocument();
  });
});

describe("MCP tool pricing", () => {
  it.each([null, {
    mode: "per_success", currency: "USD", unit_price: "0.001",
    unit_price_usd: "0.001", effective_credits: "0.001",
    charge_quota: 500, quota_per_unit: 500000, price_revision: 1,
  }])("opens explicit pricing controls and saves a price with existing pricing=%j", async (pricing) => {
    const user = userEvent.setup();
    const config = useSystemConfigStore.getState().config;
    useSystemConfigStore.getState().setConfig({
      currency: { ...config.currency, quotaDisplayType: "TOKENS" },
    });
    const tool = {
      id: "tool-1", name: "web_search", description: "Search public pages",
      revision: 3, enabled: false, input_schema: {}, output_schema: null, pricing,
    };
    request.mockResolvedValue([tool]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    try {
      render(
        <QueryClientProvider client={client}>
          <ToolPanel server={{ id: "server-1" } as MCPServer} sensitive={false} operate write />
        </QueryClientProvider>,
      );
      await user.click(await screen.findByRole("button", { name: "Edit price" }));
      const dialog = within(await screen.findByRole("dialog"));
      const price = dialog.getByRole("textbox", { name: "USD per successful call (0 is free)" });
      await user.clear(price);
      await user.type(price, "0.002");
      await user.click(dialog.getByRole("button", { name: "Save" }));
      await waitFor(() => expect(request).toHaveBeenCalledWith(
        "/servers/server-1/tools/tool-1/pricing", "PUT",
        { expected_revision: 3, mode: "per_success", unit_price: "0.002", currency: "USD" },
      ));
      await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    } finally {
      useSystemConfigStore.getState().setConfig(config);
    }
  });

  it("disables price editing without either editing permission", async () => {
    request.mockResolvedValue([{ id: "tool-1", name: "web_search", pricing: null }]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    render(
      <QueryClientProvider client={client}>
        <ToolPanel server={{ id: "server-1" } as MCPServer} sensitive={false} operate write={false} />
      </QueryClientProvider>,
    );
    expect(await screen.findByRole("button", { name: "Edit price" })).toBeDisabled();
  });

  it.each([
    ["USD", "0.001", "$0.001"],
    ["CNY", "0.001", "¥0.001"],
    ["USD", "0", "$0"],
  ])("shows only the configured %s amount %s", async (currency, unitPrice, expected) => {
    request.mockResolvedValue([{
      id: "tool-1", name: "web_search", description: "Search",
      pricing: { currency, unit_price: unitPrice, unit_price_usd: "0.000136",
        charge_quota: 68, effective_credits: "0.000136", quota_per_unit: 500000 },
    }]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    render(
      <QueryClientProvider client={client}>
        <ToolPanel server={{ id: "server-1" } as MCPServer} sensitive={false} operate write />
      </QueryClientProvider>,
    );
    expect(await screen.findByText(expected)).toBeVisible();
    expect(screen.queryByText(/Credits|quota/)).not.toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole("button", { name: "Edit price" }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.getByText(expected)).toBeVisible();
    expect(dialog.queryByText(/Credits|quota/)).not.toBeInTheDocument();
  });
});

describe("MCP marketplace", () => {
  it("shows version and sanitized README without authentication type and lists tools without calling them", async () => {
    const server = {
      id: "one",
      name: "Manual",
      version: "1.0.0",
      readme: "# Guide\n<script>bad()</script>",
      auth_mode: "oauth2",
      kind: "remote_mcp",
      installation: { id: "owned", authorized: true },
      limits: { timeout_seconds: 60 },
    };
    request.mockImplementation(async (path: string) =>
      path.endsWith("test-tools") ? [] : server,
    );
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    clients.push(client);
    const change = vi.fn();
    const page = (tab: "readme" | "tools") => (
      <QueryClientProvider client={client}>
        <ServicePage id="one" manage={false} tab={tab} onTabChange={change} />
      </QueryClientProvider>
    );
    const view = render(page("readme"));
    expect(await screen.findByRole("heading", { name: "Guide" })).toBeVisible();
    expect(screen.getByText("1.0.0")).toBeVisible();
    expect(screen.queryByText("Authentication")).not.toBeInTheDocument();
    expect(screen.queryByText("OAuth2")).not.toBeInTheDocument();
    expect(view.container.querySelector("script")).toBeNull();
    screen.getByRole("tab", { name: "Service configuration" }).focus();
    await userEvent.setup().keyboard("{ArrowRight}{Enter}");
    expect(change).toHaveBeenCalledWith("tools");
    view.rerender(page("tools"));
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith("/self/servers/one/test-tools"),
    );
    expect(request.mock.calls.every((entry) => entry[1] !== "POST")).toBe(true);
  });
  it("shows version without authentication labels and keeps connection controls without uninstall", async () => {
    const catalogItems = {
      items: [
        {
          id: "offline",
          name: "Offline",
          enabled: true,
          auth_mode: "none",
          client_connection: { type: "streamable-http", url: "https://gateway.test/mcp/offline" },
          installation: { id: "installation-2", authorized: false },
        },
        {
          id: "new-service",
          name: "New service",
          client_connection: { type: "streamable-http", url: "https://gateway.test/mcp/new-service" },
          enabled: true,
          auth_mode: "oauth2",
        },
        {
          id: "remote-server",
          version: "1.0.0",
          name: "Search",
          description: "Search tools",
          enabled: true,
          auth_mode: "oauth2",
          url: "https://gateway.test/mcp/remote-server",
          client_connection: { type: "streamable-http", url: "https://provider.test/mcp" },
          installation: { id: "installation-1", authorized: true },
        },
      ],
      total: 1,
      page: 1,
      page_size: 24,
    };
    request.mockImplementation(async (path: string, method?: string) => {
      if (method === "POST" && path === "/self/installations") return { id: "new-installation", authorized: false };
      if (method === "POST") return {};
      return catalogItems;
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    clients.push(client);
    const router = createRouter({
      routeTree: createRootRoute({ component: MCPMarketplace }),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );
    expect(await screen.findByText("Connected")).toBeVisible();
    expect(screen.getByText("1.0.0")).toBeVisible();
    expect(screen.queryByText(/OAuth2|No authentication/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Uninstall" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Authorize" })).toBeNull();
    const copyUser = userEvent.setup();
    const clipboard = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    const cards = ["Offline", "Search"].map((name) => screen.getByRole("link", { name }).closest('[data-slot="card"]') as HTMLElement);
    for (const card of cards) {
      expect(within(card).getByRole("button", { name: "Copy to clipboard" })).toBeVisible();
      expect(within(card).getByRole("button", { name: "Reset" })).toBeVisible();
      expect(within(card).getByRole("switch")).toBeVisible();
    }
    const newCard = screen.getByRole("link", { name: "New service" }).closest('[data-slot="card"]') as HTMLElement;
    expect(within(newCard).getByRole("button", { name: "Copy to clipboard" })).toBeVisible();
    expect(within(newCard).getByRole("button", { name: "Reset" })).toBeDisabled();
    expect(within(newCard).getByRole("switch")).not.toBeChecked();
    await copyUser.click(within(cards[1]).getByRole("button", { name: "Copy to clipboard" }));
    expect(JSON.parse(clipboard.mock.calls[0][0])).toEqual({
      mcpServers: { Search: { type: "streamable-http", url: "https://provider.test/mcp" } },
    });
    expect(screen.queryByRole("link", { name: "Test" })).toBeNull();
    await userEvent
      .setup()
      .click(screen.getByRole("switch", { name: "New service" }));
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith("/self/installations", "POST", {
        server_id: "new-service",
      }),
    );
    await waitFor(() => expect(request).toHaveBeenCalledWith("/self/installations/new-installation/enable", "POST"));
    expect(
      request.mock.calls.some((call) => call[0].endsWith("/authorization")),
    ).toBe(false);
    const user = userEvent.setup();
    await user.click(screen.getByRole("switch", { name: "Offline" }));
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith("/self/installations/installation-2/enable", "POST"),
    );
    expect(screen.queryByRole("dialog")).toBeNull();
    await user.click(screen.getByRole("switch", { name: "Search" }));
    expect(
      within(cards[1]).getByRole("button", { name: "Reset" }),
    ).toBeVisible();
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith(
        "/self/installations/installation-1/disable",
        "POST",
      ),
    );
    expect(request).not.toHaveBeenCalledWith("/self/installations/installation-1", "DELETE");
  });
  it.each([true, false])("resets a server with authorized=%s without starting authorization", async (authorized) => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    request.mockResolvedValue({});
    const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
    clients.push(client);
    const item = { id: "server-1", name: "Search", description: "Search tools", auth_mode: "oauth2", enabled: true };
    let connected = authorized;
    request.mockImplementation(async (_path: string, method?: string) => {
      if (method === "DELETE") connected = false;
      return { items: [{ ...item, installation: { id: "installation-1", authorized: connected } }], total: 1 };
    });
    client.setQueryData(["mcp", "catalog", "all", "", 1], { items: [{ ...item, installation: { id: "installation-1", authorized } }], total: 1 });
    const router = createRouter({
      routeTree: createRootRoute({ component: MCPMarketplace }),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    });
    render(
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );
    await userEvent.setup().click(await screen.findByRole("button", { name: "Reset" }));
    await waitFor(() => expect(request).toHaveBeenCalledWith("/self/installations/installation-1/authorization", "DELETE"));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Search" })).not.toBeChecked());
    expect(request.mock.calls.filter((call) => call[1] === "POST")).toEqual([]);
    expect(open).not.toHaveBeenCalled();
    expect(screen.queryByRole("status")).toBeNull();
    open.mockRestore();
  });
});

describe("MCP authorization return", () => {
  it.each(["REAUTHORIZATION_REQUIRED", "ENGINE_AUTHORIZATION_REQUIRED"])("starts OAuth when tool discovery returns %s", async (code) => {
    const authorizationURL = `${window.location.origin}/#knowledge-oauth`;
    request.mockImplementation(async (path: string) => {
      if (path.endsWith("/discoveries")) throw { response: { data: { code } } };
      if (path.endsWith("/discovery-authorization")) return { authorization_url: authorizationURL };
      return [];
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    clients.push(client);
    render(<QueryClientProvider client={client}><ToolPanel server={{ id: "library", kind: "public_knowledge" } as MCPServer} sensitive operate write /></QueryClientProvider>);
    await userEvent.setup().click(await screen.findByRole("button", { name: "Discover tools" }));
    await waitFor(() => expect(request).toHaveBeenCalledWith("/servers/library/discovery-authorization", "POST"));
    await waitFor(() => expect(window.location.hash).toBe("#knowledge-oauth"));
    expect(request.mock.calls.some(([path]) => path.endsWith("/tools/sync"))).toBe(false);
  });

  it("resumes discovery and synchronizes tools after the administrator OAuth callback succeeds", async () => {
    useAuthStore.getState().auth.setUser({ id: 1, username: "root", role: 100 });
    window.history.replaceState({}, "", "/mcp?server_id=library&discover=true#mcp_state=state&mcp_code=code&mcp_iss=issuer");
    const server = { id: "library", name: "Public manual", kind: "public_knowledge", enabled: false, revision: 3, connection: { transport: "streamable_http", url: "https://gateway.test/mcp/providers/knowledge/owner/manual" } };
    let authorized = false;
    request.mockImplementation(async (path: string) => {
      if (path === "/oauth/callback") { authorized = true; return { admin: true }; }
      if (path.startsWith("/servers?")) return { items: [server], total: 1, page: 1, page_size: 20 };
      if (path.endsWith("/discoveries")) {
        expect(authorized).toBe(true);
        return { discovery_id: "discovery", server_revision: 3, added: ["create_consultation", "get_consultation_result"], changed: [], removed: [] };
      }
      if (path.endsWith("/tools")) return [];
      return server;
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    clients.push(client);
    client.setQueryData(["status"], { mcp_enabled: true });
    render(<QueryClientProvider client={client}><MCPManagement /></QueryClientProvider>);
    await waitFor(() => expect(request).toHaveBeenCalledWith("/servers/library/tools/sync", "POST", { discovery_id: "discovery", expected_revision: 3 }));
    expect(request.mock.calls.filter(([path]) => path === "/oauth/callback")).toHaveLength(1);
    expect(request.mock.calls.filter(([path]) => path.endsWith("/discoveries"))).toHaveLength(1);
    expect(window.location.hash).toBe("");
    expect(new URLSearchParams(window.location.search).has("discover")).toBe(false);
  });

  it.each([
    ["popup", true], ["popup", false],
    ["marketplace", true], ["marketplace", false],
    ["detail", true], ["detail", false],
  ] as const)("returns from %s authorization with success=%s", async (source, success) => {
    const post = vi.fn();
    vi.stubGlobal("BroadcastChannel", class {
      postMessage = post;
      close = vi.fn();
    });
    const close = vi.spyOn(window, "close").mockImplementation(() => {});
    if (source !== "detail") sessionStorage.setItem("mcp-authorization-return", `${source}:one`);
    window.history.replaceState({}, "", "/mcp/servers/one?tab=tools#mcp_state=state&mcp_code=code&mcp_iss=issuer");
    const server = { id: "one", name: "Search", kind: "remote_mcp", enabled: true, auth_mode: "oauth2", installation: { id: "installed", authorized: success }, limits: { timeout_seconds: 60 } };
    request.mockImplementation(async (path: string) => {
      if (path === "/oauth/callback") {
        if (!success) throw new Error("Authorization denied");
        return {};
      }
      return path.endsWith("test-tools") ? [] : server;
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    const returned = vi.fn();
    render(<QueryClientProvider client={client}>
      <ServicePage id="one" manage={false} tab="tools" onTabChange={vi.fn()} onReturnToMarketplace={returned} />
    </QueryClientProvider>);
    await waitFor(() => expect(request).toHaveBeenCalledWith("/oauth/callback", "POST", { state: "state", code: "code", issuer: "issuer" }));
    if (!success) expect(await screen.findByRole("alert")).toHaveTextContent("Authorization denied");
    if (source === "marketplace" || source === "popup" && success) {
      await waitFor(() => expect(returned).toHaveBeenCalledWith(...(success ? [] : ["Authorization denied"])));
    } else expect(returned).not.toHaveBeenCalled();
    if (source === "popup") {
      await waitFor(() => expect(post).toHaveBeenCalledWith(success
        ? { server_id: "one", success: true }
        : { server_id: "one", success: false, error: "Authorization denied" }));
    } else expect(post).not.toHaveBeenCalled();
    expect(close).toHaveBeenCalledTimes(source === "popup" && success ? 1 : 0);
    expect(window.location.hash).toBe("");
    expect(sessionStorage.getItem("mcp-authorization-return")).toBeNull();
    expect(request.mock.calls.filter(([path]) => path === "/oauth/callback")).toHaveLength(1);
    close.mockRestore();
  });

  it("refreshes the original marketplace card from server state after popup authorization", async () => {
    const replace = vi.fn();
    const open = vi.spyOn(window, "open").mockReturnValue({ closed: false, close: vi.fn(), opener: null, location: { replace }, sessionStorage: { setItem: vi.fn() } } as unknown as Window);
    let receive: ((event: MessageEvent) => void) | null = null;
    vi.stubGlobal("BroadcastChannel", class {
      addEventListener(_event: string, handler: (event: MessageEvent) => void) { receive = handler; }
      close = vi.fn();
    });
    let authorized = false;
    const item = { id: "one", name: "Search", description: "", auth_mode: "oauth2", enabled: true };
    request.mockImplementation(async (path: string) => {
      if (path.endsWith("/enable")) return { authorization_url: "https://issuer.test/authorize" };
      const installation = { id: "installed", authorized };
      return path.startsWith("/catalog?") ? { items: [{ ...item, installation }], total: 1 } : { ...item, installation };
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    const router = createRouter({ routeTree: createRootRoute({ component: MCPMarketplace }), history: createMemoryHistory({ initialEntries: ["/"] }) });
    render(<QueryClientProvider client={client}><RouterProvider router={router} /></QueryClientProvider>);
    await userEvent.setup().click(await screen.findByRole("switch", { name: "Search" }));
    await screen.findByRole("status");
    authorized = true;
    act(() => receive?.(new MessageEvent("message", { data: { server_id: "one", success: true } })));
    await waitFor(() => expect(screen.queryByRole("status")).not.toBeInTheDocument());
    expect(screen.getByRole("switch", { name: "Search" })).toBeChecked();
    expect(replace).toHaveBeenCalledWith("https://issuer.test/authorize");
    open.mockRestore();
  });

  it("records a same-tab marketplace return when the authorization popup is blocked", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    request.mockResolvedValue({ authorization_url: "https://issuer.test/authorize" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    render(<QueryClientProvider client={client}>
      <InstallationConnection compact returnToMarketplace item={{ id: "one", name: "Search", description: "", auth_mode: "oauth2", enabled: true }} installation={{ id: "installed", authorized: false }} />
    </QueryClientProvider>);
    await userEvent.setup().click(screen.getByRole("switch", { name: "Search" }));
    await waitFor(() => expect(sessionStorage.getItem("mcp-authorization-return")).toBe("marketplace:one"));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    open.mockRestore();
  });

  it.each(["denied", "closed"])("keeps the marketplace switch off when popup authorization is %s", async (outcome) => {
    const popupStorage = new Map<string, string>();
    const replace = vi.fn();
    const popup = {
      closed: false, opener: null, close: vi.fn(), location: { replace },
      sessionStorage: { setItem: (key: string, value: string) => popupStorage.set(key, value) },
    };
    const open = vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    let receive: ((event: MessageEvent) => void) | null = null;
    vi.stubGlobal("BroadcastChannel", class {
      addEventListener(_event: string, handler: (event: MessageEvent) => void) { receive = handler; }
      close = vi.fn();
    });
    request.mockImplementation(async (path: string) => path.endsWith("/enable")
      ? { authorization_url: "https://issuer.test/authorize" }
      : { installation: { id: "installed", authorized: false } });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    render(<QueryClientProvider client={client}>
      <InstallationConnection compact returnToMarketplace item={{ id: "one", name: "Search", description: "", auth_mode: "oauth2", enabled: true }} installation={{ id: "installed", authorized: false }} />
    </QueryClientProvider>);
    await userEvent.setup().click(screen.getByRole("switch", { name: "Search" }));
    await waitFor(() => expect(replace).toHaveBeenCalledWith("https://issuer.test/authorize"));
    expect(popupStorage.get("mcp-authorization-return")).toBe("popup:one");
    await screen.findByRole("status");
    act(() => receive?.(new MessageEvent("message", { data: { server_id: "other", success: false, error: "Unrelated failure" } })));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    if (outcome === "denied") {
      act(() => receive?.(new MessageEvent("message", { data: { server_id: "one", success: false, error: "Authorization denied" } })));
      expect(await screen.findByRole("alert")).toHaveTextContent("Authorization denied");
    } else {
      popup.closed = true;
      await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Authorization required"), { timeout: 3000 });
    }
    expect(screen.getByRole("switch", { name: "Search" })).not.toBeChecked();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    open.mockRestore();
  });
});

describe("MCP real test interaction", () => {
  it.each([false, true])("renders answer chunks before completion and replaces the preview with the saved result (interrupted=%s)", async (interrupted) => {
    let update: ((event: import('../types').ConsultationUpdate) => void) | undefined;
    let end: ((value: {invocation_id: string; interrupted: boolean}) => void) | undefined;
    let signal: AbortSignal | undefined;
    let finished = false;
    streamRequest.mockImplementation((_path, _body, _key, inputSignal, accepted, onUpdate) => {
      signal = inputSignal;
      update = onUpdate;
      accepted('stream-call');
      return new Promise((resolve) => { end = resolve; });
    });
    request.mockImplementation(async (path: string) => {
      if (path.endsWith('/tools')) return [{id:'consult', name:'get_consultation_result', revision:1, input_schema:{properties:{}},pricing:{charge_quota:0}}];
      if (!finished) return {invocation:{status:'running',billing_status:'free'}};
      return {invocation:{status:'succeeded',billing_status:'free'},result:{structuredContent:{answer:'Authoritative answer',status:'completed',done:true,citations:[]}}};
    });
    const user = userEvent.setup();
    const view = renderMCPTest();
    await user.click(await screen.findByRole('button',{name:'get_consultation_result'}));
    await user.click(screen.getByRole('button',{name:'Execute'}));
    await waitFor(() => expect(streamRequest).toHaveBeenCalledOnce());
    act(() => {
      update?.({task_id:'task-one',type:'answer_delta',text:'Hello '});
      update?.({task_id:'task-one',type:'answer_delta',text:'world'});
      update?.({task_id:'task-one',type:'citation',title:'Reference',url:'https://example.test/source'});
      update?.({task_id:'task-one',type:'citation',title:'Unsafe reference',url:'javascript:alert(1)'});
    });
    expect(await within(screen.getByRole('region',{name:'Answer'})).findByText('Hello world')).toBeVisible();
    expect(screen.getByRole('link',{name:'Reference'})).toHaveAttribute('href','https://example.test/source');
    expect(screen.queryByRole('link',{name:'Unsafe reference'})).toBeNull();
    expect(screen.getByRole('button',{name:'Execute'})).toBeDisabled();
    await act(async () => end?.({invocation_id:'stream-call',interrupted}));
    if (interrupted) expect(screen.getByText('Live updates disconnected. Waiting for the saved result.')).toBeVisible();
    finished = true;
    await act(async () => view.client.refetchQueries({queryKey:['mcp','test-result','stream-call']}));
    expect(await within(screen.getByRole('region',{name:'Answer'})).findByText('Authoritative answer')).toBeVisible();
    expect(screen.queryByText('Hello world')).toBeNull();
    expect(screen.getByRole('button',{name:'Execute'})).toBeEnabled();
    expect(streamRequest).toHaveBeenCalledOnce();
    view.unmount();
    expect(signal?.aborted).toBe(true);
  });

  it("keeps only one tool open and preserves arguments across pointer and keyboard toggles", async () => {
    request.mockResolvedValue([
      { id: "fetch", name: "web_fetch", pricing: { charge_quota: 0 }, input_schema: { properties: { url: { type: "string" } }, required: ["url"] } },
      { id: "search", name: "web_search", pricing: { charge_quota: 0 }, input_schema: { properties: { count: { type: "integer" }, query: { type: "string" }, language: { type: "string" }, category: { type: "string" } }, required: ["query", "category"] } },
    ]);
    const user = userEvent.setup();
    renderMCPTest();
    const fetch = await screen.findByRole("button", { name: "web_fetch" });
    const search = screen.getByRole("button", { name: "web_search" });
    await user.click(fetch);
    await user.type(screen.getByRole("textbox", { name: "url" }), "https://example.test");
    await user.click(search);
    expect(fetch).toHaveAttribute("aria-expanded", "false");
    expect(search).toHaveAttribute("aria-expanded", "true");
    expect(screen.queryByRole("textbox", { name: "url" })).toBeNull();
    expect(screen.getAllByRole("textbox").map((field) => field.getAttribute("aria-label"))).toEqual(["query", "category", "language"]);
    const form = screen.getByRole("region", { name: "web_search" });
    expect(form).toHaveClass("transition-[height]", "h-(--accordion-panel-height)", "data-ending-style:h-0");
    expect([...form.querySelectorAll("input")].map((field) => field.name)).toEqual(["arg:query", "arg:category", "arg:count", "arg:language"]);
    expect(within(form).getByRole("button", { name: "Execute" })).toBeEnabled();
    await user.click(search);
    expect(search).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("textbox", { name: "query" })).toBeNull();
    await user.keyboard("{Enter}");
    expect(search).toHaveAttribute("aria-expanded", "true");
    await user.click(fetch);
    expect(screen.getByRole("textbox", { name: "url" })).toHaveValue("https://example.test");
    expect(search).toHaveAttribute("aria-expanded", "false");
    await user.keyboard(" ");
    expect(fetch).toHaveAttribute("aria-expanded", "false");
  });

  it.each([false, true])("shows only a spinner until polling finishes (failure=%s)", async (fails) => {
    let resolveSubmission: ((value: { invocation_id: string }) => void) | undefined;
    const submission = new Promise<{ invocation_id: string }>((resolve) => { resolveSubmission = resolve; });
    let finished = false;
    request.mockImplementation(async (path: string) => {
      if (path.endsWith("/tools")) return [{ id: "tool-1", name: "web_search", revision: 1, input_schema: { properties: {}, additionalProperties: false }, pricing: { charge_quota: 0 } }];
      if (path.endsWith("/test-invocations")) return submission;
      if (!finished) return { invocation: { status: "running", billing_status: "reserved", funding_source: "intermediate-only" } };
      if (fails) throw new Error("Result request failed");
      return { invocation: { status: "succeeded", billing_status: "settled" }, result: { answer: "Final tool result" } };
    });
    const user = userEvent.setup();
    const view = renderMCPTest();
    await user.click(await screen.findByRole("button", { name: "web_search" }));
    const execute = screen.getByRole("button", { name: "Execute" });
    await user.click(execute);
    expect(execute).toBeDisabled();
    expect(execute).toHaveAttribute("aria-busy", "true");
    expect(within(execute).getByRole("status", { name: "Running" })).toBeVisible();
    expect(screen.queryByText("Result · JSON")).toBeNull();
    await act(async () => resolveSubmission?.({ invocation_id: "pending-call" }));
    await waitFor(() => expect(request).toHaveBeenCalledWith("/test-invocations/pending-call"));
    expect(screen.queryByText(/intermediate-only/)).toBeNull();
    expect(screen.queryByText("Result · JSON")).toBeNull();
    expect(execute).toBeDisabled();
    finished = true;
    await act(async () => view.client.refetchQueries({ queryKey: ["mcp", "test-result", "pending-call"] }));
    expect(await screen.findByText(fails ? /Result request failed/ : /Final tool result/)).toBeVisible();
    expect(execute).toBeEnabled();
    expect(execute).toHaveAttribute("aria-busy", "false");
    expect(within(execute).queryByRole("status")).toBeNull();
    expect(screen.queryByText(/intermediate-only/)).toBeNull();
  });

  it("starts collapsed and replaces the inline JSON on repeat execution", async () => {
    let calls = 0;
    request.mockImplementation(async (path: string) => {
      if (path.endsWith("/tools")) return [{id:"tool-1",name:"web_search",revision:1,input_schema:{type:"object", properties:{},additionalProperties:false},pricing:{charge_quota:1,effective_credits:"1"}}];
      if (path.endsWith("/test-invocations")) return {invocation_id:"call-"+(++calls)};
      return {invocation:{status:"succeeded",billing_status:"settled"},result:{answer:path}};
    });
    const user = userEvent.setup();
    renderMCPTest();
    const toggle = await screen.findByRole("button",{name:"web_search"});
    expect(toggle).toHaveAttribute("aria-expanded","false");
    await user.click(toggle);
    expect(screen.queryByText("Result · JSON")).toBeNull();
    const execute = screen.getByRole("button",{name:"Execute"});
    await user.click(execute);
    expect(await screen.findByText(/call-1/)).toBeVisible();
    await user.click(screen.getByText("Result · JSON"));
    expect(screen.getByText("Result · JSON").closest("details")).not.toHaveAttribute("open");
    await user.click(execute);
    expect(await screen.findByText(/call-2/)).toBeVisible();
    expect(screen.queryByText(/call-1/)).toBeNull();
    expect(calls).toBe(2);
  });

  it.each([1, 2])(
    "only loads saved test results owned by the current user (owner %s)",
    async (owner) => {
      useAuthStore
        .getState()
        .auth.setUser({ id: 1, username: "root", role: 100 });
      const invocation = {
        id: "saved-call",
        user_id: owner,
        is_test: true,
        tool_name: "web_search",
        status: "succeeded",
        billing_status: "settled",
        charged_quota: 500,
        quota_per_unit: "500000",
        created_at: "2026-11-15T11:58:21Z",
      };
      request.mockImplementation(async (path: string) => {
        if (path.startsWith("/invocations?")) {
          return { items: [invocation], total: 1 };
        }
        if (path.startsWith("/usage")) return { items: [] };
        if (path === "/invocations/saved-call") {
          return { invocation, billing_operations: [] };
        }
        if (path === "/test-invocations/saved-call") {
          return {
            invocation,
            result: {
              content: [{ type: "text", text: "Saved search result" }],
            },
          };
        }
        throw new Error(`Unexpected request: ${path}`);
      });
      const client = new QueryClient({
        defaultOptions: { queries: { retry: false } },
      });
      clients.push(client);
      render(
        <QueryClientProvider client={client}>
          <InvocationList />
        </QueryClientProvider>,
      );
      await userEvent
        .setup()
        .click(await screen.findByRole("button", { name: "web_search" }));
      const dialog = await screen.findByRole("dialog", {
        name: "Invocation details",
      });
      expect(await within(dialog).findByText("saved-call")).toBeVisible();
      if (owner === 1) {
        expect(
          await within(dialog).findByText(/Saved search result/),
        ).toBeVisible();
      } else {
        expect(
          request.mock.calls.some(
            (call) => call[0] === "/test-invocations/saved-call",
          ),
        ).toBe(false);
      }
      expect(
        request.mock.calls.every((call) => !call[1] || call[1] === "GET"),
      ).toBe(true);
    },
  );

  it("requires the operator OAuth authorization", async () => {
    request.mockImplementation(async (path: string) =>
      path.endsWith("/test-authorization") ? { authorized: false } : [],
    );
    renderMCPTest("casdoor_oauth");
    expect(
      await screen.findByText(
        "Authorize your own Casdoor account before testing.",
      ),
    ).toBeInTheDocument();
    expect(request.mock.calls.every((call) => call[1] !== "POST")).toBe(true);
  });

  it("shows the frozen price and reuses the key after an uncertain submission", async () => {
    let submitted = 0;
    request.mockImplementation(async (path: string) => {
      if (path.endsWith("/tools")) {
        return [
          {
            id: "tool-1",
            name: "web_search",
            revision: 4,
            input_schema: {
              type: "object",
              required: ["query"],
              properties: {
                query: { type: "string" },
                count: { type: "integer", default: 3 },
                need_content: { type: "boolean" },
                offset: { type: "integer" },
              },
            },
            pricing: { charge_quota: 500, effective_credits: "0.001" },
          },
        ];
      }
      if (path.endsWith("/test-invocations")) {
        submitted++;
        if (submitted === 1) throw new Error("Network unavailable");
        return { invocation_id: "call-1" };
      }
      if (path.endsWith("/call-1")) {
        return {
          invocation: {
            status: "failed",
            billing_status: "refund_pending",
            error_code: "UPSTREAM_FAILED",
          },
        };
      }
      throw new Error(`Unexpected request: ${path}`);
    });
    const user = userEvent.setup();
    renderMCPTest();
    await user.click(await screen.findByRole("button",{name:"web_search"}));
    expect(screen.getByLabelText("query")).toBeRequired();
    expect(screen.getByLabelText("offset")).not.toBeRequired();
    await user.type(screen.getByLabelText("query"), "DeepSeek API docs");
    await user.selectOptions(screen.getByLabelText("need_content"), "false");
    await user.click(screen.getByRole("button", { name: "Execute" }));
    expect(await screen.findByText(/Network unavailable/)).toBeVisible();
    expect(screen.getByText(/0.001 Credits/)).toBeInTheDocument();
    await user.click(screen.getByRole("button",{name:"Execute"}));
    await waitFor(() => expect(submitted).toBe(2));
    const calls = request.mock.calls.filter((call) =>
      call[0].endsWith("/test-invocations"),
    );
    expect(calls[0][2]).toEqual({
      tool_name: "web_search",
      arguments: { query: "DeepSeek API docs", count: 3, need_content: false },
      expected_tool_revision: 4,
      max_charge_quota: 500,
    });
    expect(calls[0][3]).toBeTruthy();
    expect(calls[1][3]).toBe(calls[0][3]);
    expect(screen.queryByText("Result · JSON")).toBeNull();
    expect(screen.getByRole("status", { name: "Running" })).toBeVisible();
    expect(screen.getByRole("button",{name:"Execute"})).toBeDisabled();
  });
});

describe("MCP configuration and charge confirmation", () => {
  it("creates a service with a single connection form and no provider fields", async () => {
    request.mockResolvedValue({
      id: "new-service",
      callback_url: "https://gateway.test/api/mcp/oauth/callback",
    });
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    clients.push(client);
    const created = vi.fn();
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={client}>
        <ServerForm sensitive onClose={vi.fn()} onCreated={created} />
      </QueryClientProvider>,
    );
    await user.type(screen.getByLabelText("Display name"), "Remote tools");
    await user.type(screen.getByLabelText("Description"), "A tool directory");
    await user.type(screen.getByLabelText("Version"), "2.1");
    await user.type(
      screen.getByLabelText("Upstream URL"),
      "https://service.test/mcp",
    );
    expect(screen.queryByLabelText("Authentication")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Channel")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Client Secret")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(created).toHaveBeenCalledWith("new-service"));
    expect(request).toHaveBeenCalledWith("/servers", "POST", {
      name: "Remote tools",
      description: "A tool directory",
      version: "2.1",
      expected_revision: undefined,
      connection: {
        url: "https://service.test/mcp",
        transport: "streamable_http",
      },
      limits: { timeout_seconds: 60, max_concurrency: 10 },
    });
  });
  it("registers a fixed public library through the standard MCP URL form", async () => {
    request.mockImplementation(async (_path, method) =>
      method === "POST" ? { id: "library-service" } : { callback_url: "https://gateway.test/api/mcp/oauth/callback" },
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    const created = vi.fn();
    const user = userEvent.setup();
    render(<QueryClientProvider client={client}><ServerForm sensitive onClose={() => {}} onCreated={created} /></QueryClientProvider>);
    await user.type(screen.getByLabelText("Display name"), "Public manual");
    await user.type(screen.getByLabelText("Upstream URL"), "https://gateway.test/mcp/providers/knowledge/library-owner/manual");
    expect(screen.queryByLabelText("Library owner")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Knowledge base ID")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(created).toHaveBeenCalledWith("library-service"));
    expect(request).toHaveBeenCalledWith("/servers", "POST", expect.objectContaining({
      connection: { transport: "streamable_http", url: "https://gateway.test/mcp/providers/knowledge/library-owner/manual" },
    }));
  });
  it("keeps the library binding fixed while editing its display name", async () => {
    request.mockResolvedValue({ callback_url: "https://gateway.test/api/mcp/oauth/callback" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    const closed = vi.fn();
    const user = userEvent.setup();
    const endpoint = "https://gateway.test/mcp/providers/knowledge/library-owner/manual";
    render(<QueryClientProvider client={client}><ServerForm sensitive onClose={closed} server={{
      id: "library-service", name: "Manual", kind: "public_knowledge", revision: 1,
      connection: { transport: "streamable_http", url: endpoint, owner: "library-owner", kb_id: "manual" },
      limits: { timeout_seconds: 60, max_concurrency: 10 },
    } as MCPServer} /></QueryClientProvider>);
    expect(screen.getByLabelText("Upstream URL")).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "Transport" })).toBeDisabled();
    await user.clear(screen.getByLabelText("Display name"));
    await user.type(screen.getByLabelText("Display name"), "Public manual");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(closed).toHaveBeenCalled());
    expect(request).toHaveBeenCalledWith("/servers/library-service", "PATCH", expect.objectContaining({
      name: "Public manual", connection: { transport: "streamable_http", url: endpoint },
    }));
  });
  it.each(["", "replacement-secret"])("saves automatic scope discovery with client secret input %j", async (secret) => {
    request.mockResolvedValue({ callback_url: "https://gateway.test/api/mcp/oauth/callback" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    clients.push(client);
    const closed = vi.fn();
    const user = userEvent.setup();
    render(
      <QueryClientProvider client={client}>
        <ServerForm sensitive onClose={closed} server={{
          id: "scope-server", name: "Remote tools", description: "", version: "1", revision: 3,
          kind: "remote_mcp", credentials_configured: true, connection: { transport: "streamable_http", url: "https://service.test/mcp", client_id: "registered", scopes: "user" },
          limits: { timeout_seconds: 60, max_concurrency: 10 },
        } as MCPServer} />
      </QueryClientProvider>,
    );
    await user.click(screen.getByText("Advanced settings"));
    await screen.findByLabelText("Client ID");
    expect(screen.queryByLabelText(/OAuth scopes/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Clear client secret" })).not.toBeInTheDocument();
    if (secret) await user.type(screen.getByLabelText("Client Secret"), secret);
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(closed).toHaveBeenCalled());
    expect(request).toHaveBeenCalledWith("/servers/scope-server", "PATCH", expect.objectContaining({
      connection: { transport: "streamable_http", url: "https://service.test/mcp", client_id: "registered", scopes: "" },
    }));
    const saved = request.mock.calls.find(([path, method]) => path === "/servers/scope-server" && method === "PATCH")?.[2];
    if (secret) {
      expect(saved).toHaveProperty("client_secret", secret);
    } else {
      expect(saved).not.toHaveProperty("client_secret");
    }
  });
  it("binds real test approval to the displayed tool revision and charge", () => {
    const tool = {
      name: "web_search",
      revision: 4,
      pricing: { charge_quota: 500 },
    } as MCPTool;
    expect(testRequest(tool, '{"query":"test"}')).toEqual({
      tool_name: "web_search",
      arguments: { query: "test" },
      expected_tool_revision: 4,
      max_charge_quota: 500,
    });
    for (const input of ["null", "[]", "invalid"]) {
      expect(() => testRequest(tool, input)).toThrow();
    }
    expect(() => testRequest({ ...tool, pricing: null }, "{}")).toThrow();
  });
  it("keeps watching refunds after execution has failed", () => {
    expect(
      testPending({
        invocation: { status: "failed", billing_status: "refund_pending" },
      } as never),
    ).toBe(true);
    expect(
      testPending({
        invocation: { status: "failed", billing_status: "refunded" },
      } as never),
    ).toBe(false);
    expect(
      testPending({
        invocation: { status: "succeeded", billing_status: "settled" },
      } as never),
    ).toBe(false);
  });
});

it("saves edited metadata and README text without authentication or README import controls", async () => {
  const server = {
    id: "edit-service", name: "Remote service", description: "Before",
    version: "1.0.0", kind: "remote_mcp", auth_mode: "none", revision: 2,
    connection: { transport: "streamable_http", url: "https://example.test/mcp" },
    limits: { timeout_seconds: 60, max_concurrency: 10 },
  } as MCPServer;
  request.mockResolvedValue(server);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  clients.push(client);
  const close = vi.fn();
  render(<QueryClientProvider client={client}><ServerForm server={server} sensitive onClose={close} /></QueryClientProvider>);
  const user = userEvent.setup();
  expect(screen.queryByLabelText("Authentication")).not.toBeInTheDocument();
  expect(screen.queryByLabelText("Import Markdown")).not.toBeInTheDocument();
  expect(screen.getByLabelText("Upload PNG icon")).toBeInTheDocument();
  await user.type(screen.getByRole("textbox", { name: "README" }), "# Updated service guide");
  await user.clear(screen.getByLabelText("Description"));
  await user.type(screen.getByLabelText("Description"), "After");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(close).toHaveBeenCalled());
  expect(request).toHaveBeenCalledWith("/servers/edit-service", "PATCH", expect.objectContaining({ description: "After", readme: "# Updated service guide", expected_revision: 2 }));
  const saved = request.mock.calls.find(([path]) => path === "/servers/edit-service");
  expect(saved?.[2]).not.toHaveProperty("auth_mode");
});

describe('MCP database configuration', () => {
  it('shows configuration state without returning secrets and disables saving for read-only users', async () => {
    const { MCPConfiguration } = await import('../settings')
    request.mockImplementation(async(path:string)=>path==='/settings/providers'?[]:({
      revision: 1,
      values: { oauth_client_id: 'verifier' },
      effective: { public_base_url: 'https://gateway.test' },
      secrets: { oauth_client_secret: true },
    }))
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        <MCPConfiguration sensitive={false} />
      </QueryClientProvider>
    )
    expect(await screen.findByDisplayValue('verifier')).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
    expect(screen.getByLabelText('Resource verifier secret')).toHaveValue('')
  })
  it('offers only HTTP transports when creating services', async () => {
    const user = userEvent.setup()
    request.mockResolvedValue([])
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        <ServerForm sensitive onClose={() => {}} />
      </QueryClientProvider>
    )
    const combo = screen.getAllByRole('combobox')[0]
    await user.click(combo)
    expect(
      screen.queryByRole('option', { name: 'stdio' })
    ).not.toBeInTheDocument()
  })
})
