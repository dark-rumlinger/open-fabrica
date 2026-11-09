/**
 * Request options for the first-run setup calls. When the server asked for a
 * setup token (admin panel reachable from the network, no admin yet) it rides in
 * a header on every setup call; otherwise there are no extra options.
 */
export function setupAuth(setupData) {
  return setupData && setupData.setupToken
    ? { headers: { 'X-Setup-Token': setupData.setupToken } }
    : undefined;
}
