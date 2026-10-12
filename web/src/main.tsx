import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from "@tanstack/react-router";
import { LoginPage } from "@/routes/login";
import { HomePage } from "@/routes/home";
import { AccessRulesPage } from "@/routes/access-rules";
import { ModelsPage } from "@/routes/models";
import { ServersPage } from "@/routes/servers";
import { SSOPage } from "@/routes/sso";
import { ConnectionsPage } from "@/routes/connections";
import { OffboardingPage } from "@/routes/offboarding";
import { AppsPage } from "@/routes/apps";
import { AppPresetPage } from "@/routes/app-preset";
import { AuditPage, UsagePage } from "@/routes/audit";
import { BudgetsPage } from "@/routes/budgets";
import { DependenciesPage } from "@/routes/dependencies";
import { KeysPage } from "@/routes/keys";
import { SettingsKeysPage } from "@/routes/settings-keys";
import "@fontsource/ibm-plex-sans/400.css";
import "@fontsource/ibm-plex-sans/500.css";
import "@fontsource/ibm-plex-sans/600.css";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";
import "./index.css";

const rootRoute = createRootRoute({ component: Outlet });
const routeTree = rootRoute.addChildren([
  createRoute({ getParentRoute: () => rootRoute, path: "/", component: HomePage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/login", component: LoginPage }),
  createRoute({
    getParentRoute: () => rootRoute, path: "/access-rules", component: AccessRulesPage,
    validateSearch: (s: Record<string, unknown>): { rule?: string; edit?: string } => ({
      rule: typeof s.rule === "string" ? s.rule : undefined,
      edit: typeof s.edit === "string" ? s.edit : undefined,
    }),
  }),
  createRoute({ getParentRoute: () => rootRoute, path: "/models", component: ModelsPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/servers", component: ServersPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/settings/sign-in", component: SSOPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/connections", component: ConnectionsPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/settings/offboarding", component: OffboardingPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/settings/apps", component: AppsPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/settings/apps/add/$id", component: AppPresetPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/keys", component: KeysPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/settings/keys", component: SettingsKeysPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/audit", component: AuditPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/usage", component: UsagePage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/budgets", component: BudgetsPage }),
  createRoute({ getParentRoute: () => rootRoute, path: "/settings/dependencies", component: DependenciesPage }),
]);
const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={new QueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
