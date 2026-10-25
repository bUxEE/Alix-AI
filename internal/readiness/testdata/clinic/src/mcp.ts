import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import * as z from "zod";

export const createMcpServer = (): McpServer => {
  const server = new McpServer({ name: "clinic-booking", version: "1.0.0" });
  server.registerTool(
    "availability_list",
    {
      title: "List free slots",
      description: "List the free appointment slots of the caller's clinic site.",
      inputSchema: { date: z.string().optional() },
    },
    async () => ({ content: [] }),
  );
  server.registerTool(
    "appointments_book_prepare",
    {
      title: "Prepare a booking",
      description: "Validate a booking and return the prepared action.",
      inputSchema: { patient: z.string(), slot: z.string() },
    },
    async () => ({ content: [] }),
  );
  return server;
};
