<script lang="ts">
	import { onMount, onDestroy, tick } from 'svelte';
	import {
		api, connectSSE, subUrl, subQrUrl, fmtBytes, fmtTime,
		type Client, type ProcessState, type LogLine, type SSEEvent
	} from '$lib/api';

	// ── State ────────────────────────────────────────────────────────────────
	let clients: Client[] = [];
	let states: Record<string, ProcessState> = {};
	let liveLogs: Record<string, string[]> = {}; // client_id → recent lines from SSE
	let loading = true;
	let error = '';

	// Modals
	let showCreate = false;
	let showQR: { id: string; token: string } | null = null;
	let showLogs: string | null = null;
	let logsContent: string[] = [];
	let logsEl: HTMLElement;

	// Create-client form
	let newName = '';
	let newSubdomain = '';
	let newRoomID = '';
	let newComment = '';
	let newSpeedMbps = 0;
	let newTrafficGB = 0;
	let newExpiresAt = '';
	let creating = false;

	// Live stats (updated by SSE)
	let liveRunning = 0;
	let liveStopped = 0;

	// ── Data loading ─────────────────────────────────────────────────────────
	async function loadAll() {
		try {
			const [c, s] = await Promise.all([api.listClients(), api.state()]);
			clients = c;
			applyStates(s);
		} catch (e: any) {
			if (e.message === 'unauthorized') {
				window.location.href = '/login';
			} else {
				error = e.message;
			}
		} finally {
			loading = false;
		}
	}

	function applyStates(s: ProcessState[]) {
		const m: Record<string, ProcessState> = {};
		let running = 0, stopped = 0;
		for (const ps of s) {
			m[ps.client_id] = ps;
			if (ps.running) running++; else stopped++;
		}
		states = m;
		liveRunning = running;
		liveStopped = stopped;
	}

	// ── SSE ──────────────────────────────────────────────────────────────────
	let sseDisconnect: (() => void) | null = null;

	function startSSE() {
		sseDisconnect = connectSSE((evt: SSEEvent) => {
			if (evt.type === 'state') {
				applyStates(evt.data as ProcessState[]);
			} else if (evt.type === 'log') {
				const { client_id, t, line } = evt.data as { client_id: string; t: string; line: string };
				const ts = new Date(t).toLocaleTimeString();
				const entry = `${ts} ${line}`;
				// Keep last 200 lines per client in live buffer
				const prev = liveLogs[client_id] ?? [];
				liveLogs[client_id] = [...prev.slice(-199), entry];
				// If logs modal is open for this client, update display
				if (showLogs === client_id) {
					logsContent = liveLogs[client_id];
					tick().then(() => {
						if (logsEl) logsEl.scrollTop = logsEl.scrollHeight;
					});
				}
			}
		});
	}

	// ── Client actions ───────────────────────────────────────────────────────
	async function clientAction(
		id: string,
		action: 'start' | 'stop' | 'restart' | 'rotate-key' | 'rotate-room' | 'delete'
	) {
		error = '';
		try {
			switch (action) {
				case 'start':       await api.startClient(id); break;
				case 'stop':        await api.stopClient(id); break;
				case 'restart':     await api.restartClient(id); break;
				case 'rotate-key':  await api.rotateKey(id); break;
				case 'rotate-room': await api.rotateRoom(id); break;
				case 'delete':
					if (!confirm('Delete client and stop its process?')) return;
					await api.deleteClient(id);
					await loadAll();
					return;
			}
		} catch (e: any) {
			error = e.message;
		}
	}

	async function createClient() {
		creating = true;
		try {
			await api.createClient({
				name: newName,
				subdomain: newSubdomain,
				room_id: newRoomID,
				comment: newComment || undefined,
				quota: {
					speed_mbps: newSpeedMbps || undefined,
					traffic_gb: newTrafficGB || undefined,
					expires_at: newExpiresAt ? new Date(newExpiresAt).toISOString() : undefined
				}
			});
			showCreate = false;
			newName = newSubdomain = newRoomID = newComment = newExpiresAt = '';
			newSpeedMbps = newTrafficGB = 0;
			await loadAll();
		} catch (e: any) {
			error = e.message;
		} finally {
			creating = false;
		}
	}

	// ── Logs ─────────────────────────────────────────────────────────────────
	async function openLogs(id: string) {
		showLogs = id;
		// Seed from REST first, then SSE will keep appending
		try {
			const res = await api.logs(id);
			logsContent = res.logs.map((l: LogLine) => `${new Date(l.t).toLocaleTimeString()} ${l.line}`);
			// Merge any live lines we already received via SSE
			if (liveLogs[id]) {
				const liveSet = new Set(liveLogs[id]);
				for (const l of logsContent) liveSet.delete(l);
				logsContent = [...logsContent, ...liveLogs[id].filter(l => !logsContent.includes(l))];
			}
		} catch {}
		await tick();
		if (logsEl) logsEl.scrollTop = logsEl.scrollHeight;
	}

	// ── Helpers ──────────────────────────────────────────────────────────────
	function statusColor(id: string): string {
		const s = states[id];
		if (!s) return 'text-gray-500';
		if (s.running) return 'text-emerald-400';
		if (s.exit_err) return 'text-red-400';
		return 'text-gray-500';
	}

	function statusText(id: string): string {
		const s = states[id];
		if (!s) return 'unknown';
		if (s.running) return 'running';
		if (s.exit_err) return 'error';
		return 'stopped';
	}

	function copyText(text: string) {
		navigator.clipboard.writeText(text);
	}

	function trafficPct(c: Client): number {
		if (!c.quota.traffic_gb || !c.quota.used_bytes) return 0;
		return Math.min(100, (c.quota.used_bytes / (c.quota.traffic_gb * 1024 ** 3)) * 100);
	}

	// ── Lifecycle ────────────────────────────────────────────────────────────
	onMount(async () => {
		const me = await api.me().catch(() => null);
		if (!me) { window.location.href = '/login'; return; }
		if (!me.configured) { window.location.href = '/setup'; return; }
		await loadAll();
		startSSE();
	});

	onDestroy(() => {
		sseDisconnect?.();
	});
</script>

<svelte:head><title>Panel</title></svelte:head>

<div class="min-h-screen bg-gray-950 text-gray-100 flex flex-col">

	<!-- ── Header ── -->
	<header class="border-b border-gray-800 px-6 py-3 flex items-center justify-between shrink-0">
		<div class="flex items-center gap-4">
			<span class="text-lg font-bold text-white tracking-tight">Panel</span>
			<!-- Live stats pills -->
			<span class="text-xs px-2 py-0.5 rounded-full bg-emerald-900/60 text-emerald-300 border border-emerald-700">
				▲ {liveRunning} running
			</span>
			{#if liveStopped > 0}
				<span class="text-xs px-2 py-0.5 rounded-full bg-red-900/40 text-red-400 border border-red-800">
					▼ {liveStopped} stopped
				</span>
			{/if}
		</div>
		<div class="flex items-center gap-2">
			<button
				on:click={() => (showCreate = true)}
				class="px-3 py-1.5 bg-blue-600 hover:bg-blue-500 rounded-lg text-sm font-medium transition-colors"
			>
				+ New
			</button>
			<button
				on:click={async () => { await api.logout(); window.location.href = '/login'; }}
				class="px-3 py-1.5 bg-gray-800 hover:bg-gray-700 rounded-lg text-sm transition-colors"
			>
				Logout
			</button>
		</div>
	</header>

	<!-- ── Error banner ── -->
	{#if error}
		<div class="mx-6 mt-3 p-3 bg-red-900/40 border border-red-700 rounded-lg text-sm text-red-300 flex items-center justify-between">
			<span>{error}</span>
			<button class="ml-3 text-red-400 hover:text-red-200" on:click={() => (error = '')}>✕</button>
		</div>
	{/if}

	<!-- ── Main ── -->
	<main class="flex-1 px-6 py-5 overflow-x-auto">
		{#if loading}
			<div class="flex items-center justify-center h-40 text-gray-600">Loading…</div>
		{:else if clients.length === 0}
			<div class="flex flex-col items-center justify-center h-60 text-gray-600 gap-3">
				<span class="text-5xl">🔒</span>
				<p>No clients yet. Click <b class="text-gray-400">+ New</b> to create one.</p>
			</div>
		{:else}
			<table class="w-full text-sm border-separate border-spacing-y-1">
				<thead>
					<tr class="text-xs text-gray-500 uppercase tracking-wide">
						<th class="text-left pb-2 pr-3 font-medium">Client</th>
						<th class="text-left pb-2 pr-3 font-medium">Room</th>
						<th class="text-left pb-2 pr-3 font-medium">Status</th>
						<th class="text-left pb-2 pr-3 font-medium w-36">Traffic</th>
						<th class="text-left pb-2 font-medium">Actions</th>
					</tr>
				</thead>
				<tbody>
					{#each clients as c (c.id)}
						<tr class="bg-gray-900/40 hover:bg-gray-900/70 rounded-lg transition-colors">
							<td class="py-3 pr-3 pl-3 rounded-l-lg">
								<div class="font-medium text-white">{c.name}</div>
								{#if c.comment}
									<div class="text-xs text-gray-500 mt-0.5">{c.comment}</div>
								{/if}
								<div class="text-xs text-gray-600 mt-0.5 font-mono">#{c.id}</div>
							</td>

							<td class="py-3 pr-3 font-mono text-xs text-gray-400 leading-5">
								<span class="text-gray-300">{c.room.subdomain}</span>.ktalk.ru<br />
								<span class="text-gray-500">{c.room.room_id}</span>
							</td>

							<td class="py-3 pr-3">
								<div class="flex items-center gap-1.5">
									<span class="w-2 h-2 rounded-full {states[c.id]?.running ? 'bg-emerald-400 animate-pulse' : 'bg-gray-600'}"></span>
									<span class="font-medium {statusColor(c.id)}">{statusText(c.id)}</span>
									{#if (states[c.id]?.restarts ?? 0) > 0}
										<span class="text-xs text-yellow-500 ml-1">↺{states[c.id].restarts}</span>
									{/if}
								</div>
								{#if states[c.id]?.started_at}
									<div class="text-xs text-gray-600 mt-0.5">
										since {new Date(states[c.id].started_at ?? '').toLocaleTimeString()}
									</div>
								{/if}
							</td>

							<td class="py-3 pr-3">
								<div class="text-xs text-gray-400">
									{fmtBytes(c.quota.used_bytes ?? 0)}
									{#if c.quota.traffic_gb} / {c.quota.traffic_gb} GB {/if}
								</div>
								{#if c.quota.traffic_gb}
									<div class="mt-1 h-1.5 w-28 bg-gray-800 rounded-full overflow-hidden">
										<div
											class="h-full rounded-full transition-all {trafficPct(c) > 90 ? 'bg-red-500' : 'bg-blue-500'}"
											style="width:{trafficPct(c)}%"
										></div>
									</div>
								{/if}
								{#if c.quota.expires_at}
									<div class="text-xs text-gray-600 mt-0.5">exp {c.quota.expires_at.slice(0,10)}</div>
								{/if}
							</td>

							<td class="py-3 pr-3 rounded-r-lg">
								<div class="flex flex-wrap gap-1">
									{#if states[c.id]?.running}
										<button
											on:click={() => clientAction(c.id, 'stop')}
											class="px-2 py-1 bg-yellow-800/70 hover:bg-yellow-700 rounded text-xs transition-colors"
										>stop</button>
										<button
											on:click={() => clientAction(c.id, 'restart')}
											class="px-2 py-1 bg-gray-700 hover:bg-gray-600 rounded text-xs transition-colors"
										>restart</button>
									{:else}
										<button
											on:click={() => clientAction(c.id, 'start')}
											class="px-2 py-1 bg-emerald-800/70 hover:bg-emerald-700 rounded text-xs transition-colors"
										>start</button>
									{/if}
									<button
										on:click={() => openLogs(c.id)}
										class="px-2 py-1 bg-gray-800 hover:bg-gray-700 rounded text-xs transition-colors"
									>logs</button>
									<button
										on:click={() => (showQR = { id: c.id, token: c.sub_token })}
										class="px-2 py-1 bg-gray-800 hover:bg-gray-700 rounded text-xs transition-colors"
									>QR</button>
									<button
										on:click={() => copyText(subUrl(c.id, c.sub_token))}
										class="px-2 py-1 bg-gray-800 hover:bg-gray-700 rounded text-xs transition-colors"
										title="Copy subscription URL"
									>copy</button>
									<button
										on:click={() => clientAction(c.id, 'rotate-key')}
										class="px-2 py-1 bg-gray-800 hover:bg-gray-700 rounded text-xs transition-colors"
										title="Rotate encryption key"
									>🔑</button>
									<button
										on:click={() => clientAction(c.id, 'delete')}
										class="px-2 py-1 bg-red-900/60 hover:bg-red-800 rounded text-xs transition-colors"
									>del</button>
								</div>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		{/if}
	</main>
</div>

<!-- ══ QR modal ══ -->
{#if showQR}
	<!-- svelte-ignore a11y-click-events-have-key-events -->
	<!-- svelte-ignore a11y-no-static-element-interactions -->
	<div class="fixed inset-0 bg-black/75 flex items-center justify-center z-50" on:click|self={() => (showQR = null)}>
		<div class="bg-gray-900 border border-gray-700 rounded-2xl p-6 max-w-sm w-full mx-4 text-center shadow-2xl">
			<h2 class="text-base font-bold mb-4">Subscription</h2>
			<img
				src={subQrUrl(showQR.id, showQR.token)}
				alt="QR code"
				class="mx-auto rounded-lg bg-white p-2"
				width="200" height="200"
			/>
			<p class="mt-3 text-xs text-gray-500 break-all">{subUrl(showQR.id, showQR.token)}</p>
			<div class="mt-4 flex gap-2 justify-center">
				<button
					on:click={() => showQR && copyText(subUrl(showQR.id, showQR.token))}
					class="px-4 py-2 bg-blue-600 hover:bg-blue-500 rounded-lg text-sm"
				>Copy link</button>
				<button
					on:click={() => (showQR = null)}
					class="px-4 py-2 bg-gray-800 hover:bg-gray-700 rounded-lg text-sm"
				>Close</button>
			</div>
		</div>
	</div>
{/if}

<!-- ══ Create client modal ══ -->
{#if showCreate}
	<!-- svelte-ignore a11y-click-events-have-key-events -->
	<!-- svelte-ignore a11y-no-static-element-interactions -->
	<div class="fixed inset-0 bg-black/75 flex items-center justify-center z-50" on:click|self={() => (showCreate = false)}>
		<div class="bg-gray-900 border border-gray-700 rounded-2xl p-6 max-w-lg w-full mx-4 shadow-2xl">
			<h2 class="text-base font-bold mb-5">New client</h2>
			<form on:submit|preventDefault={createClient} class="space-y-4">
				<div class="grid grid-cols-2 gap-3">
					<div>
						<label class="text-xs text-gray-400 block mb-1">Name *</label>
						<input bind:value={newName} required autocomplete="off"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
					<div>
						<label class="text-xs text-gray-400 block mb-1">Comment</label>
						<input bind:value={newComment} autocomplete="off"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
				</div>
				<div class="grid grid-cols-2 gap-3">
					<div>
						<label class="text-xs text-gray-400 block mb-1">Subdomain *</label>
						<input bind:value={newSubdomain} required placeholder="ilte0310" autocomplete="off"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
					<div>
						<label class="text-xs text-gray-400 block mb-1">Room ID *</label>
						<input bind:value={newRoomID} required placeholder="abcdef123456" autocomplete="off"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
				</div>
				<div class="grid grid-cols-3 gap-3">
					<div>
						<label class="text-xs text-gray-400 block mb-1">Speed Mbps</label>
						<input type="number" bind:value={newSpeedMbps} min="0"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
					<div>
						<label class="text-xs text-gray-400 block mb-1">Traffic GB</label>
						<input type="number" bind:value={newTrafficGB} min="0"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
					<div>
						<label class="text-xs text-gray-400 block mb-1">Expires</label>
						<input type="date" bind:value={newExpiresAt}
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
				</div>
				<div class="flex gap-3 pt-1">
					<button
						type="submit" disabled={creating}
						class="flex-1 py-2 bg-blue-600 hover:bg-blue-500 disabled:opacity-50 rounded-lg text-sm font-medium"
					>{creating ? 'Creating…' : 'Create'}</button>
					<button type="button" on:click={() => (showCreate = false)}
						class="px-4 py-2 bg-gray-800 hover:bg-gray-700 rounded-lg text-sm"
					>Cancel</button>
				</div>
			</form>
		</div>
	</div>
{/if}

<!-- ══ Logs modal ══ -->
{#if showLogs}
	<!-- svelte-ignore a11y-click-events-have-key-events -->
	<!-- svelte-ignore a11y-no-static-element-interactions -->
	<div
		class="fixed inset-0 bg-black/75 flex items-center justify-center z-50"
		on:click|self={() => { showLogs = null; logsContent = []; }}
	>
		<div class="bg-gray-900 border border-gray-700 rounded-2xl p-4 w-full max-w-3xl mx-4 max-h-[80vh] flex flex-col shadow-2xl">
			<div class="flex items-center justify-between mb-3">
				<div>
					<h2 class="font-bold text-sm">Logs</h2>
					<p class="text-xs text-gray-500 mt-0.5">
						Live via SSE · client <code class="text-gray-400">{showLogs}</code>
					</p>
				</div>
				<button
					on:click={() => { showLogs = null; logsContent = []; }}
					class="text-gray-500 hover:text-gray-200 text-2xl leading-none"
				>×</button>
			</div>
			<div
				bind:this={logsEl}
				class="flex-1 overflow-y-auto bg-gray-950 rounded-xl p-3 font-mono text-xs leading-5 space-y-0.5"
			>
				{#if logsContent.length === 0}
					<div class="text-gray-600">No logs yet…</div>
				{:else}
					{#each logsContent as line}
						<div class="text-gray-300 whitespace-pre-wrap break-all">{line}</div>
					{/each}
				{/if}
			</div>
		</div>
	</div>
{/if}
