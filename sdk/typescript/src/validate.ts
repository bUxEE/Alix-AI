import { sdkError } from "./errors.js";

// Local validation that runs before anything is sent. A malformed argument must never cost a
// single-use context token or reach the wire, so every check here happens before getContextToken.

const PATH_ID = /^[A-Za-z0-9_.-]{1,128}$/;

/**
 * A session, preparation or application id as it appears in a path: the pattern of the OpenAPI
 * document. "." and ".." match the pattern but URL normalisation would rewrite the path, so they
 * are refused too.
 */
export const pathId = (kind: string, value: unknown): string => {
  if (typeof value !== "string" || !PATH_ID.test(value) || value === "." || value === "..") {
    throw sdkError("validation_failed", `${kind} is not a valid identifier`, { details: { argument: kind } });
  }
  return value;
};

/** A file id as `POST /v1/files` returns it: the pattern of the OpenAPI document. */
export const FILE_ID = /^fil_[0-9A-HJKMNP-TV-Z]{26}$/;

const CONFIRMATION_TOKEN = /^cfm_[A-Za-z0-9_-]{43}$/;

export const isConfirmationToken = (value: unknown): value is string =>
  typeof value === "string" && CONFIRMATION_TOKEN.test(value);

/** The confirmation secret as a request argument. The value is never echoed in the error. */
export const confirmationToken = (value: unknown): string => {
  if (!isConfirmationToken(value)) {
    throw sdkError("validation_failed", "the confirmation token is malformed", { details: { argument: "token" } });
  }
  return value;
};

// A bearer credential is an RFC 7235 token68: letters, digits and - . _ ~ + / =. The context token (a
// compact JWS today) fits. Anything else (whitespace, a "Bearer " prefix, a newline that would split
// the header) is refused before it is sent anywhere.
const TOKEN68 = /^[A-Za-z0-9._~+/=-]+$/;

export const contextToken = (value: unknown): string => {
  if (typeof value !== "string" || value.length === 0 || value.length > 16 * 1024 || !TOKEN68.test(value)) {
    throw sdkError("unauthenticated", "getContextToken must return the bare context token (no whitespace, no \"Bearer\" prefix)", {
      details: { reason: "bad_token_shape" },
    });
  }
  return value;
};

const LOOPBACK_V4 = /^127(\.\d{1,3}){3}$/;

const isLoopbackHost = (hostname: string): boolean =>
  hostname === "localhost" || hostname.endsWith(".localhost") || LOOPBACK_V4.test(hostname) || hostname === "[::1]";

/**
 * The base URL without a trailing slash, path prefix kept ("https://host/alixai"). The context token
 * travels as a bearer credential, so plain http is accepted only for a loopback host unless the
 * caller opts in; credentials in the URL, a query and a fragment are refused.
 */
export const normalizeBaseUrl = (input: unknown, allowInsecureHttp: boolean): string => {
  if (typeof input !== "string" || input.trim() === "") {
    throw sdkError("validation_failed", "baseUrl is required", { details: { argument: "baseUrl" } });
  }
  let url: URL;
  try {
    url = new URL(input.trim());
  } catch (cause) {
    throw sdkError("validation_failed", "baseUrl is not an absolute URL", { details: { argument: "baseUrl" }, cause });
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw sdkError("validation_failed", "baseUrl must be http or https", { details: { argument: "baseUrl" } });
  }
  if (url.username !== "" || url.password !== "" || url.search !== "" || url.hash !== "") {
    throw sdkError("validation_failed", "baseUrl must not carry credentials, a query or a fragment", {
      details: { argument: "baseUrl" },
    });
  }
  if (url.protocol === "http:" && !allowInsecureHttp && !isLoopbackHost(url.hostname)) {
    throw sdkError(
      "validation_failed",
      "baseUrl must be https: the context token is a bearer credential (http is accepted only for loopback; set allowInsecureHttp to override)",
      { details: { argument: "baseUrl" } },
    );
  }
  const path = url.pathname.replace(/\/+$/, "");
  return `${url.protocol}//${url.host}${path}`;
};
