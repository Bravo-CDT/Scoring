/**
 * Saves blob as a file called name. The files need the bearer token, so they
 * are fetched first and handed over through an object URL rather than linked.
 */
export function saveBlob(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.append(a);
  a.click();
  a.remove();
  // Revoked later: some browsers start the download after click returns.
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}
