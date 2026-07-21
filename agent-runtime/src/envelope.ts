/**
 * Response envelope: `{ ok, data?, error? }` (backend dev-design §3.4).
 */

export type ErrorCode =
  | "unauthorized"
  | "not_found"
  | "invalid_request"
  | "invalid_session_key"
  | "timeout"
  | "internal";

export interface OkEnvelope<T> {
  ok: true;
  data: T;
}

export interface ErrEnvelope {
  ok: false;
  error: { code: ErrorCode; message: string };
}

export type Envelope<T> = OkEnvelope<T> | ErrEnvelope;

export function ok<T>(data: T): OkEnvelope<T> {
  return { ok: true, data };
}

export function err(code: ErrorCode, message: string): ErrEnvelope {
  return { ok: false, error: { code, message } };
}

/** HTTP status for each error code. */
export const STATUS_BY_CODE: Record<ErrorCode, number> = {
  unauthorized: 401,
  not_found: 404,
  invalid_request: 400,
  invalid_session_key: 400,
  timeout: 504,
  internal: 500,
};
