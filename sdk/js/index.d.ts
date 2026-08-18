export interface QueryResult {
  columns: Array<{ name: string; type: string }>;
  rows: unknown[][];
  snapshot_id: number;
  truncated: boolean;
}

export interface Client {
  query(name: string, parameters?: Record<string, string | number | boolean>): Promise<QueryResult>;
}

export declare function createClient(baseURL?: string): Client;
