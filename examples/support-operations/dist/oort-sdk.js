export function createClient(baseURL = "") {
  return {
    async query(name, parameters = {}) {
      const response = await fetch(`${baseURL}/runtime/v1/queries/${encodeURIComponent(name)}`, {
        method: "POST",
        credentials: "same-origin",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({parameters}),
      });
      if (!response.ok) {
        const problem = await response.json().catch(() => null);
        throw new Error(problem?.error || `Query failed (${response.status})`);
      }
      return response.json();
    },
  };
}
