/** "Default" while on the config's credentials, else when the blue team changed them. */
export function changedLabel(changedAt: string | undefined): string {
  if (!changedAt) return 'Default';
  return `Changed ${new Date(changedAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;
}
