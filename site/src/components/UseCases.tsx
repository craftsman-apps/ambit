import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import styles from '../pages/index.module.css';

const CASES = [
  {title: 'Coding tools', to: '/docs/guides/coding-tools', text: 'Give Claude Code and Cursor a personal API key instead of a shared provider key.'},
  {title: 'Models per team', to: '/docs/guides/models-per-team', text: 'Allow each identity-provider group only the models it needs.'},
  {title: 'MCP servers for work apps', to: '/docs/guides/mcp-apps', text: 'Let people connect their own app accounts, and allow only the tools you choose.'},
  {title: 'Spending limits', to: '/docs/guides/spending-limits', text: 'Set a daily limit per group and per person, in requests or in dollars.'},
  {title: 'When someone leaves', to: '/docs/guides/someone-leaves', text: 'Disable the person in your identity provider. Their keys, sessions and app sign-ins stop at once.'},
];

export default function UseCases(): ReactNode {
  return (
    <ul className={styles.cards}>
      {CASES.map((c) => (
        <li key={c.to}>
          <Link className={styles.card} to={c.to}>
            <h3>{c.title}</h3>
            <p>{c.text}</p>
          </Link>
        </li>
      ))}
    </ul>
  );
}
