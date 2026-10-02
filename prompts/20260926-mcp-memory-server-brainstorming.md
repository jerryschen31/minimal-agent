 ▐▛███▛█   Claude Code v2.1.283
▝▜██████▀  Sonnet 5 · Claude Pro
 ▝▝   ▝▝   ~/gh/others/mcp/servers/src/memory

⚠ Your login expires in 2 days · run /login to renew

  Get to finished work sooner with Opus 5.5. Switch anytime with /model.

❯ Curious about the memory/ MCP server - persistent memory as a knowledge       
  graph.                                                                        
  1. Is the knowledge graph stored as a single JSON? Multiple JSONs? Some other 
  structure?                                                                    
  2. Is the knowledge graph persisted as JSON files, or something else? And if  
  I installed this MCP server and started using it as my persistent memory      
  storage, HOW would I store the persistent memory files?                       

  Read 1 file, ran 1 shell command (ctrl+o to expand)

⏺ 1. Structure: one file, in JSON Lines format.

  The whole graph lives in a single file, memory.jsonl. It has one JSON object
  per line, and each line is either an entity or a relation (index.ts:162-176):

  jsonl
  {"type":"entity","name":"Jerry","entityType":"person","observations":["prefers
  TypeScript","works on MCP servers"]}
  {"type":"entity","name":"MCP","entityType":"protocol","observations":[]}
  {"type":"relation","from":"Jerry","to":"MCP","relationType":"works_with"}

  - Entities have a name, an entityType and a list of observations, which are
    plain-text facts.
  - Relations are directed edges with from, to and relationType.
  - Older versions used a single memory.json. On startup the server renames it
    to memory.jsonl if the new file doesn't exist yet (index.ts:29-62).

  2. Persistence and where the file goes.

  The graph is persisted as that plain-text file, with no database. Behavior
  worth knowing:

  - Load: every tool call re-reads and parses the whole file. Malformed or
    schema-invalid lines are skipped with a log message instead of failing the
    load (index.ts:109-160). A missing file is treated as an empty graph.
  - Save: every mutation rewrites the entire file. The server writes to a
    random-named .tmp file in the same directory, then atomically renames it
    over the real file, so a crash mid-write can't leave a truncated graph
    (index.ts:178-199).
  - Concurrency: mutations are serialized through an in-process queue
    (withLock). This only protects a single server process. Two separate server
    instances pointed at the same file could still overwrite each other.

  Where it's stored:

  - Default: memory.jsonl in the server's own directory, meaning next to the
    compiled index.js. With npx, that's inside the npx cache. That location is
    easy to lose track of and may be wiped, so I wouldn't rely on it for real
    use.
  - Recommended: set the MEMORY_FILE_PATH env var in your MCP client config.
    Absolute paths and a leading ~ are supported. Relative paths resolve against
    the package directory, not your working directory.

  {
    "mcpServers": {
      "memory": {
        "command": "npx",
        "args": ["-y", "@modelcontextprotocol/server-memory"],
        "env": { "MEMORY_FILE_PATH": "~/.claude/memory.jsonl" }
      }
    }
  }

  - Docker: the README's example mounts a named volume (claude-memory:/app/dist)
    so the file survives container restarts.

  Because it's plain JSONL, you can back it up, put it in git, or edit it by
  hand. Just don't edit it while the server is running, because the next write
  will overwrite your changes. You also need to point every client (Claude
  Desktop, Claude Code and so on) at the same MEMORY_FILE_PATH if you want them
  to share one memory.

✻ Crunched for 14s · done 3:23 PM

※ recap: You asked how the memory MCP server stores its knowledge graph, and I answered: a single JSONL file, rewritten atomically on each change, with its location set via MEMORY_FILE_PATH. Next, decide where you want your memory file to live and add that env var to your client config. (disable recaps in /config)

❯ Interesting. I think saving the entire persistent memory as a single file,    
  and a mutation rewriting the entire file, can be improved on. And perhaps a   
  JSON-supporting database like MongoDB might be best as an improvement to this 
  MCP server.                                                                   

⏺ The concern is valid, but I'd pick a different fix than MongoDB.

  What's wrong today
  - Write amplification: adding one observation reads and rewrites the whole
    graph, so cost grows with total memory size.
  - Read cost: every read tool, including search_nodes and open_nodes, parses
    the full file first.
  - Single-process locking: the in-process queue does nothing across multiple
    server instances. That is a real problem when several clients share one
    memory file.
  - No indexes: lookups by name, and search, scan everything.

  For a personal memory holding hundreds to low thousands of entities this is
  fine in practice. It only hurts as the graph grows or when several clients
  write at once.

  Why I wouldn't use MongoDB
  - It needs a running server or a hosted service, which breaks the "npx it and
    go" setup this package has now.
  - The data isn't document-shaped. It's a graph of entities and edges, and
    relations reference entities by name. That fits relational tables better
    than documents.
  - It adds a heavy dependency to a reference server. The repo's contributing
    guidelines are selective about "highly opinionated additions" and features
    beyond a server's core purpose. A Mongo requirement would probably be
    rejected upstream.

  Better options
  1. SQLite (better-sqlite3 or Node 22's built-in node:sqlite) is what I'd
     choose.
     - Setup: it's embedded and zero-config, still one file on disk, and needs
       no server.
     - Schema: entities, observations and relations tables with a unique
       constraint on entity name and foreign keys with cascade delete. That
       would also replace the manual dedupe and cleanup logic.
     - Writes: each mutation touches only the affected rows, in a transaction.
     - Concurrency: it handles multiple processes correctly, through WAL mode
       and file locking.
     - Search: indexes or FTS5 replace the in-memory scan.
  2. Append-only JSONL log plus periodic compaction. This is the smallest
     change. Writes append add/delete operations and the file is compacted
     occasionally. It keeps the human-readable format, but cross-process safety
     and indexing stay weak.
  3. Pluggable storage backend. Put a Storage interface behind
     KnowledgeGraphManager with JSONL as the default. SQLite, Mongo or Postgres
     could then be optional adapters. This is the most upstream-friendly route,
     and your Mongo idea would fit as one adapter.

  Trade-offs of SQLite
  - You lose the ability to hand-edit or git diff the memory file.
  - The native module (better-sqlite3) complicates installs and Docker builds.
    node:sqlite avoids that, but I'd need to check its stability on the Node 22
    version we target before committing.
  - Existing users need a one-time migration from memory.jsonl. This can be
    automatic, similar to the existing memory.json to memory.jsonl rename.

  I can sketch option 3 with a SQLite adapter, or prototype the SQLite backend
  on a branch with tests. Which do you want, or would you rather keep going on
  Mongo?

✻ Churned for 14s · done 3:43 PM