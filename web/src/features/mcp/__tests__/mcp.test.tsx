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
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useAuthStore } from "@/stores/auth-store";

import { MCPManagement } from "..";
import { InvocationList } from "../invocations";
import { InstallationConnection } from "../installation-connection";
import { MCPMarketplace } from "../marketplace";
import { ServerForm } from "../server-form";
import { ServicePage } from "../service-page";
import { TestPanel } from "../test-panel";
import {
  testPending,
  testRequest,
  type MCPServer,
  type MCPTool,
} from "../types";

const request = vi.hoisted(() => vi.fn());
vi.mock("../api", () => ({ mcpRequest: request }));
const clients: QueryClient[] = [];
afterEach(() => {
  for (const client of clients) client.clear();
  clients.length = 0;
  request.mockReset();
  useAuthStore.getState().auth.reset();
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

describe("MCP marketplace", () => {
  it("opens a sanitized README and lists tools without calling them", async () => {
    const server = {
      id: "one",
      name: "Manual",
      readme: "# Guide\n<script>bad()</script>",
      auth_mode: "none",
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
  it("keeps marketplace authorization controls and reveals secondary actions on interaction", async () => {
    request.mockResolvedValue({
      items: [
        {
          id: "offline",
          name: "Offline",
          enabled: true,
          auth_mode: "none",
          installation: { id: "installation-2", authorized: false },
        },
        {
          id: "new-service",
          name: "New service",
          enabled: true,
          auth_mode: "oauth2",
        },
        {
          id: "remote-server",
          name: "Search",
          description: "Search tools",
          enabled: true,
          auth_mode: "oauth2",
          url: "https://gateway.test/mcp/remote-server",
          installation: { id: "installation-1", authorized: true },
        },
      ],
      total: 1,
      page: 1,
      page_size: 24,
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
    expect(screen.queryByRole("button", { name: "Authorize" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Copy connection configuration" })).toBeNull();
    const uninstall = screen.getAllByRole("button", { name: "Uninstall" })[1];
    expect(uninstall).toHaveClass("text-destructive");
    expect(uninstall).toHaveClass("pointer-events-none", "opacity-0", "group-hover/card:opacity-100", "group-focus-within/card:opacity-100", "[@media(hover:none)]:opacity-100");
    expect(screen.getByRole("link", { name: "Test" })).toHaveAttribute(
      "href",
      "/mcp/servers/remote-server?tab=tools",
    );
    await userEvent
      .setup()
      .click(screen.getByRole("button", { name: "Install" }));
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith("/self/installations", "POST", {
        server_id: "new-service",
      }),
    );
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
      screen.getByRole("button", { name: "Reset" }),
    ).toBeVisible();
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() =>
      expect(request).toHaveBeenCalledWith(
        "/self/installations/installation-1/disable",
        "POST",
      ),
    );
    uninstall.focus();
    expect(uninstall).toHaveFocus();
    await user.keyboard("{Enter}");
    const confirmation = await screen.findByRole("alertdialog");
    expect(request).not.toHaveBeenCalledWith("/self/installations/installation-1", "DELETE");
    await user.click(within(confirmation).getByRole("button", { name: "Uninstall" }));
    await waitFor(() => expect(request).toHaveBeenCalledWith("/self/installations/installation-1", "DELETE"));
  });
  it.each([true, false])("resets a server with authorized=%s by revoking its grant before starting authorization", async (authorized) => {
    const popup = {
      closed: false,
      opener: null,
      sessionStorage: window.sessionStorage,
      location: { replace: vi.fn() },
      close: vi.fn(),
    };
    vi.spyOn(window, "open").mockReturnValue(popup as unknown as Window);
    request.mockResolvedValue({ authorization_url: "https://provider.test/authorize" });
    const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
    clients.push(client);
    render(
      <QueryClientProvider client={client}>
        <InstallationConnection
          compact
          item={{ id: "server-1", name: "Search", description: "Search tools", auth_mode: "oauth2", enabled: true }}
          installation={{ id: "installation-1", authorized }}
        />
      </QueryClientProvider>,
    );
    await userEvent.setup().click(screen.getByRole("button", { name: "Reset" }));
    await waitFor(() => expect(popup.location.replace).toHaveBeenCalledWith("https://provider.test/authorize"));
    expect(request.mock.calls.slice(0, 2)).toEqual([
      ["/self/installations/installation-1/authorization", "DELETE"],
      ["/self/installations/installation-1/authorization", "POST"],
    ]);
    expect(screen.getByRole("status")).toHaveTextContent("Waiting for browser authorization");
    expect(screen.queryByRole("button", { name: "Reset" })).toBeNull();
    window.sessionStorage.removeItem("mcp-return-installation");
  });
});

describe("MCP real test interaction", () => {
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

it("saves edited remote service metadata without an authentication selector", async () => {
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
  await user.clear(screen.getByLabelText("Description"));
  await user.type(screen.getByLabelText("Description"), "After");
  await user.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(close).toHaveBeenCalled());
  expect(request).toHaveBeenCalledWith("/servers/edit-service", "PATCH", expect.objectContaining({ description: "After", expected_revision: 2 }));
  const saved = request.mock.calls.find(([path]) => path === "/servers/edit-service");
  expect(saved?.[2]).not.toHaveProperty("auth_mode");
});
