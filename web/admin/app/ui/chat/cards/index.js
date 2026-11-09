/**
 * Chat-card public surface. Routes a tool call to the appropriate
 * per-card-type renderer; returns null to fall through to the generic
 * tool-call card in message.js.
 */

import {
  createArtifactCard,
  createAssetCard,
  createLinkAssetCard,
  createProjectTreeCard,
} from './artifact-card.js';
import { createTableCard } from './table-card.js';
import { createEndpointCard } from './endpoint-card.js';
import { createFlowsCard } from './flows-card.js';

export { setPanelSwitcher, getToolLabel } from './shared.js';

// Actions that meaningfully *touch* (write or read) a single artifact and
// deserve the rich page/asset card with its "View …" deep-link button.
// Read-only actions (get/outline) get the same card so the user sees which
// file is being inspected; project_tree / list are too broad and fall through
// to the generic card.
const artifactCardActions = new Set([
  'save', 'patch',
  'delete', 'rename',
  'get', 'outline',
]);

export function createToolCard(toolName, status, args, result) {
  const action = args?.action;

  switch (toolName) {
    case 'manage_files':
      if (action === 'project_tree') {
        return createProjectTreeCard(toolName, status, args, result);
      }
      if (artifactCardActions.has(action)) {
        const kind = args?.kind;
        if (kind === 'page' || kind === 'layout' || kind === 'component') {
          return createArtifactCard(toolName, status, args, result);
        }
        if (kind === 'file') {
          return createAssetCard(toolName, status, args, result);
        }
      }
      if (action === 'attach' || action === 'detach') {
        return createLinkAssetCard(toolName, status, args, result);
      }
      return null;

    case 'manage_data':
      return createTableCard(toolName, status, args, result);

    case 'manage_testing':
      // Only the user flow actions get a result card; the page and endpoint
      // probes stay on the generic card.
      if (action === 'write_flows' || action === 'test_flows') {
        return createFlowsCard(toolName, status, args, result);
      }
      return null;

    case 'manage_agent':
    case 'manage_api':
    case 'manage_auth':
    case 'manage_relay':
    case 'manage_tick':
    case 'manage_authoritative':
    case 'manage_matchmaking':
    case 'manage_signaling':
    case 'manage_sse':
    case 'manage_llm_proxy':
      return createEndpointCard(toolName, status, args, result);

    case 'manage_realtime':
      // get/delete target one endpoint and earn the rich endpoint card; list
      // spans every model and carries no endpoint_path, so let it fall through
      // to the generic card rather than render a misleading "/api/endpoint".
      if (action === 'list') return null;
      return createEndpointCard(toolName, status, args, result);

    default:
      return null;
  }
}
