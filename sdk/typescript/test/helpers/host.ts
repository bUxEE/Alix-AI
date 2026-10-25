import { createClient } from "../../src/index.js";
import type { AlixAIClient, ClientOptions, ContextTokenRequest } from "../../src/index.js";
import { createFakeServer } from "../../src/testing/index.js";
import type { FakeAlixAIServer, FakeServerOptions, FakeTokenClaims } from "../../src/testing/index.js";

/** A host application in miniature: a fake AlixAI, and a client whose token provider mints one fresh token per call. */
export interface Host {
  fake: FakeAlixAIServer;
  client: AlixAIClient;
  /** Every request the SDK made of the token provider. */
  tokenRequests: ContextTokenRequest[];
}

export const createHost = (
  claims: Partial<FakeTokenClaims> = {},
  options: FakeServerOptions = {},
  clientOptions: Omit<Partial<ClientOptions>, "baseUrl" | "getContextToken" | "fetch" | "webSocket"> & { webSocket?: boolean } = {},
): Host => {
  const { webSocket, ...rest } = clientOptions;
  const fake = createFakeServer(options);
  const tokenRequests: ContextTokenRequest[] = [];
  const client = createClient({
    baseUrl: fake.baseUrl,
    fetch: fake.fetch,
    getContextToken: (request) => {
      tokenRequests.push(request);
      return fake.issueToken({ session_id: "ses-1", ...claims });
    },
    ...(webSocket === true && { webSocket: fake.webSocket }),
    ...rest,
  });
  return { fake, client, tokenRequests };
};

/** Drains an async iterable into an array. */
export const collect = async <T>(iterable: AsyncIterable<T>): Promise<T[]> => {
  const out: T[] = [];
  for await (const item of iterable) out.push(item);
  return out;
};

export const bytes = (text: string): Uint8Array => new TextEncoder().encode(text);

/** A ReadableStream that delivers the given chunks, one per read. */
export const streamOf = (chunks: Uint8Array[]): ReadableStream<Uint8Array> => {
  let i = 0;
  return new ReadableStream<Uint8Array>({
    pull: (controller) => {
      const chunk = chunks[i++];
      if (chunk === undefined) controller.close();
      else controller.enqueue(chunk);
    },
  });
};

/** Splits bytes into chunks of at most `size` bytes. */
export const split = (data: Uint8Array, size: number): Uint8Array[] => {
  const out: Uint8Array[] = [];
  for (let i = 0; i < data.length; i += size) out.push(data.slice(i, i + size));
  return out;
};
