import type {ReactNode} from 'react';
import Layout from '@theme/Layout';
import Link from '@docusaurus/Link';
import ThemedImage from '@theme/ThemedImage';
import useBaseUrl from '@docusaurus/useBaseUrl';
import UseCases from '../components/UseCases';
import styles from './index.module.css';

const REPO = 'https://github.com/craftsman-apps/ambit';

const WHY = [
  ['Each team shares one provider API key, copied into scripts and laptops.', 'People sign in, or use a personal API key. Provider keys stay inside Ambit.'],
  ['Anyone who has the key can use any model.', 'Access rules based on your identity provider\'s groups decide which models and MCP servers each team may use.'],
  ['AI agents connect to work apps on their own, often through one shared service account.', 'Every MCP call passes one gateway. People connect their own app accounts, tools are checked, and personal data in results is masked.'],
  ['The cost shows up on next month\'s bill.', 'Daily limits per group and per person, in requests or in dollars.'],
  ['Nobody can say who sent what to which model.', 'Every call is recorded, denied calls included.'],
  ['Someone leaves, and their keys work until someone remembers to rotate them.', 'The identity provider disables them, and their keys, sessions and app sign-ins stop at once.'],
];

const BUILT_ON = [
  ['agentgateway', 'https://github.com/agentgateway/agentgateway', 'LLM and MCP traffic', 'Apache-2.0'],
  ['ToolHive', 'https://github.com/stacklok/toolhive', 'Runs MCP servers', 'Apache-2.0'],
  ['Dex', 'https://github.com/dexidp/dex', 'Sign-in and tokens', 'Apache-2.0'],
  ['Gateway API', 'https://github.com/kubernetes-sigs/gateway-api', 'Routing objects', 'Apache-2.0'],
  ['ClickHouse', 'https://github.com/ClickHouse/ClickHouse', 'Audit storage', 'Apache-2.0'],
  ['OpenTelemetry Collector', 'https://github.com/open-telemetry/opentelemetry-collector-contrib', 'Audit pipeline', 'Apache-2.0'],
  ['Valkey', 'https://github.com/valkey-io/valkey', 'Budget counters', 'BSD-3-Clause'],
  ['envoyproxy/ratelimit', 'https://github.com/envoyproxy/ratelimit', 'Budget enforcement', 'Apache-2.0'],
  ['MCP Go SDK', 'https://github.com/modelcontextprotocol/go-sdk', 'Preset checks and test MCP servers', 'Apache-2.0 (older code MIT)'],
];

function Themed({name, alt, className}: {name: string; alt: string; className?: string}): ReactNode {
  return (
    <ThemedImage
      className={className}
      alt={alt}
      sources={{light: useBaseUrl(`/brand/${name}.svg`), dark: useBaseUrl(`/brand/${name}-dark.svg`)}}
    />
  );
}

export default function Home(): ReactNode {
  return (
    <Layout
      title="The AI access layer for enterprises"
      description="Ambit is a self-hosted gateway that decides who may use which AI models and work apps, sets spending limits, keeps an audit record, and removes access when someone leaves.">
      <main className={styles.page}>
        <header className={styles.hero}>
          <div>
            <p className={styles.eyebrow}>Alpha · v0.1.0 · Apache-2.0</p>
            <h1>The AI access layer for enterprises.</h1>
            <p className={styles.lead}>
              A self-hosted gateway that decides who may use which AI models and work apps, sets spending limits,
              keeps an audit record, and removes access when someone leaves.
            </p>
            <div className={styles.buttons}>
              <Link className={styles.primary} to="/docs/quick-start">Quick start</Link>
              <Link className={styles.secondary} href={REPO}>GitHub</Link>
            </div>
          </div>
          <ThemedImage
            className={styles.shot}
            alt="The Ambit console, Audit page: every model call one person made, with time, model, outcome and tokens."
            sources={{light: useBaseUrl('/img/console-light.png'), dark: useBaseUrl('/img/console-dark.png')}}
          />
        </header>

        <section>
          <h2>Why Ambit</h2>
          <Themed
            name="ambit-why"
            className={styles.diagram}
            alt="Without Ambit: Alice, Dan and Bob share one team API key and call models directly, an AI agent reaches work apps through a shared service account, Bob's key still works after he left, there is no record and the cost appears on next month's bill. With Ambit: everyone passes the Ambit gateway, which checks who they are, what they may use and their daily limit, records every call, and refuses Bob."
          />
          <div className={styles.tableWrap}>
            <table>
              <thead><tr><th>Without Ambit</th><th>With Ambit</th></tr></thead>
              <tbody>{WHY.map(([a, b]) => <tr key={a}><td>{a}</td><td>{b}</td></tr>)}</tbody>
            </table>
          </div>
        </section>

        <section>
          <h2>Use cases</h2>
          <UseCases />
        </section>

        <section>
          <h2>Architecture</h2>
          <Themed
            name="ambit-architecture"
            className={styles.diagram}
            alt="Request path: clients call one Ambit gateway, built on agentgateway, which checks the caller and their access, asks the budget service, guard, token broker and router as needed, then forwards to AI models or MCP servers and sends audit records through the OpenTelemetry Collector to ClickHouse. Setup: your identity provider signs people in through Dex and reports leavers to the console by SCIM; admins use the console, which stores Ambit objects; ambit-controller turns them into gateway configuration."
          />
          <p className={styles.key}>Blue is the path every request takes; grey is how admins and your identity provider set things up.</p>
        </section>

        <section>
          <h2>Built on</h2>
          <p>Ambit is built on these projects. Each keeps its own licence.</p>
          <div className={styles.tableWrap}>
            <table>
              <thead><tr><th>Project</th><th>Use in Ambit</th><th>Licence</th></tr></thead>
              <tbody>
                {BUILT_ON.map(([name, href, use, licence]) => (
                  <tr key={name}><td><Link href={href}>{name}</Link></td><td>{use}</td><td>{licence}</td></tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      </main>
    </Layout>
  );
}
