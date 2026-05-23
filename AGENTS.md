
## Node Package Manager
- For Node-based workspaces in this repository, the preferred package manager is `pnpm` (use `corepack pnpm ...` by default).

## Message Readability
- Emojify reports/messages where it improves readability, using relevant emojis only.

## Implementation Language
- Use Rust as much as possible, and write everything in Rust whenever practical. Avoid shell scripts unless absolutely necessary. For web-based work, or anything that requires a web interface, use SvelteKit/Svelte with TypeScript or JavaScript when it cannot be handled cleanly in Rust.
