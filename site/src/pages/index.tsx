import type {ReactNode} from 'react';
import Layout from '@theme/Layout';

// Placeholder: the landing page is built in a later step.
export default function Home(): ReactNode {
  return (
    <Layout title="Ambit" description="The AI access layer for enterprises.">
      <main className="container margin-vert--xl">
        <h1>Ambit</h1>
        <p>The AI access layer for enterprises.</p>
        <a href="/docs/">Documentation</a>
      </main>
    </Layout>
  );
}
