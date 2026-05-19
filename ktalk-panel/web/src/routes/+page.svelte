<script lang="ts">
	import { onMount, onDestroy } from 'svelte';
	import { api, subUrl, subQrUrl, type Client, type ProcessState } from '$lib/api';

	let clients: Client[] = [];
	let states: Record<string, ProcessState> = {};
	let loading = true;
	let error = '';
	let showCreate = false;
	let showQR: string | null = null;
	let showLogs: string | null = null;
	let logsContent: string[] = [];
	let logsTimer: ReturnType<typeof setInterval>;

	// New client form
	let newName = '';
	let newSubdomain = 'ilte0310';
	let newRoomID = '';
	let newComment = '';
	let newSpeedMbps = 0;
	let newTrafficGB = 0;
	let newExpiresAt = '';
	let creating = false;

	async function load() {
		try {
			const [c, s] = await Promise.all([api.listClients(), api.state()]);
			clients = c;
			const stateMap: Record<string, ProcessState> = {};
			for (const ps of s) stateMap[ps.client_id] = ps;
			states = stateMap;
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

	async function createClient() {
		creating = true;
		try {
			await api.createClient({
				name: newName,
				subdomain: newSubdomain,
				room_id: newRoomID,
				comment: newComment,
				quota: {
					speed_mbps: newSpeedMbps || undefined,
					traffic_gb: newTrafficGB || undefined,
					expires_at: newExpiresAt || undefined
				}
			});
			showCreate = false;
			newName = newRoomID = newComment = newExpiresAt = '';
			newSpeedMbps = newTrafficGB = 0;
			await load();
		} catch (e: any) {
			error = e.message;
		} finally {
			creating = false;
		}
	}

	async function clientAction(id: string, action: 'start' | 'stop' | 'restart' | 'rotate-key' | 'rotate-room' | 'delete') {
		error = '';
		try {
			switch (action) {
				case 'start': await api.startClient(id); break;
				case 'stop': await api.stopClient(id); break;
				case 'restart': await api.restartClient(id); break;
				case 'rotate-key': await api.rotateKey(id); break;
				case 'rotate-room': await api.rotateRoom(id); break;
				case 'delete':
					if (!confirm('Delete client and stop its process?')) return;
					await api.deleteClient(id);
					break;
			}
			await load();
		} catch (e: any) {
			error = e.message;
		}
	}

	async function openLogs(id: string) {
		showLogs = id;
		await refreshLogs();
		logsTimer = setInterval(refreshLogs, 3000);
	}

	async function refreshLogs() {
		if (!showLogs) return;
		try {
			const res = await api.logs(showLogs);
			logsContent = res.logs.map(l => `${new Date(l.t).toLocaleTimeString()} ${l.line}`);
		} catch {}
	}

	function closeLogs() {
		clearInterval(logsTimer);
		showLogs = null;
		logsContent = [];
	}

	function copyToClipboard(text: string) {
		navigator.clipboard.writeText(text);
	}

	function statusColor(id: string): string {
		const s = states[id];
		if (!s) return 'text-gray-500';
		if (s.running) return 'text-green-400';
		return 'text-red-400';
	}

	function statusText(id: string): string {
		const s = states[id];
		if (!s) return 'unknown';
		if (s.running) return 'running';
		if (s.exit_err) return 'error';
		return 'stopped';
	}

	let pollTimer: ReturnType<typeof setInterval>;
	onMount(async () => {
		const me = await api.me().catch(() => null);
		if (!me?.configured) { window.location.href = '/setup'; return; }
		await load();
		pollTimer = setInterval(load, 5000);
	});
	onDestroy(() => { clearInterval(pollTimer); clearInterval(logsTimer); });
</script>

<svelte:head><title>ktalk panel</title></svelte:head>

<div class="min-h-screen bg-gray-950 text-gray-100">
	<!-- Header -->
	<header class="border-b border-gray-800 px-6 py-4 flex items-center justify-between">
		<h1 class="text-xl font-bold tracking-tight text-white">⚡ ktalk panel</h1>
		<div class="flex gap-3">
			<button
				on:click={() => (showCreate = true)}
				class="px-4 py-2 bg-blue-600 hover:bg-blue-500 rounded-lg text-sm font-medium transition-colors"
			>
				+ New client
			</button>
			<button
				on:click={async () => { await api.logout(); window.location.href = '/login'; }}
				class="px-4 py-2 bg-gray-800 hover:bg-gray-700 rounded-lg text-sm transition-colors"
			>
				Logout
			</button>
		</div>
	</header>

	<!-- Error banner -->
	{#if error}
		<div class="mx-6 mt-4 p-3 bg-red-900/50 border border-red-700 rounded-lg text-sm text-red-300">
			{error}
			<button class="ml-2 underline" on:click={() => (error = '')}>dismiss</button>
		</div>
	{/if}

	<!-- Main content -->
	<main class="px-6 py-6">
		{#if loading}
			<p class="text-gray-500">Loading…</p>
		{:else if clients.length === 0}
			<div class="text-center py-20 text-gray-600">
				<p class="text-4xl mb-4">🔒</p>
				<p>No clients yet. Create one to get started.</p>
			</div>
		{:else}
			<div class="overflow-x-auto">
				<table class="w-full text-sm">
					<thead>
						<tr class="border-b border-gray-800 text-gray-500 text-left">
							<th class="pb-3 pr-4 font-medium">Name</th>
							<th class="pb-3 pr-4 font-medium">Room</th>
							<th class="pb-3 pr-4 font-medium">Status</th>
							<th class="pb-3 pr-4 font-medium">Traffic</th>
							<th class="pb-3 pr-4 font-medium">Quota</th>
							<th class="pb-3 font-medium">Actions</th>
						</tr>
					</thead>
					<tbody>
						{#each clients as c}
							<tr class="border-b border-gray-900 hover:bg-gray-900/30">
								<td class="py-3 pr-4">
									<div class="font-medium text-white">{c.name}</div>
									{#if c.comment}<div class="text-xs text-gray-500">{c.comment}</div>{/if}
								</td>
								<td class="py-3 pr-4 font-mono text-xs text-gray-400">
									{c.room.subdomain}.ktalk.ru<br />{c.room.room_id}
								</td>
								<td class="py-3 pr-4">
									<span class="font-medium {statusColor(c.id)}">{statusText(c.id)}</span>
									{#if states[c.id]?.restarts > 0}
										<span class="ml-1 text-xs text-yellow-500">↺{states[c.id].restarts}</span>
									{/if}
								</td>
								<td class="py-3 pr-4 text-xs text-gray-400">
									{((c.quota.used_bytes ?? 0) / 1e9).toFixed(2)} GB
									{#if c.quota.traffic_gb} / {c.quota.traffic_gb} GB {/if}
								</td>
								<td class="py-3 pr-4 text-xs text-gray-400">
									{#if c.quota.speed_mbps}{c.quota.speed_mbps} Mbps{:else}∞{/if}<br />
									{#if c.quota.expires_at}exp: {c.quota.expires_at.slice(0,10)}{:else}no exp{/if}
								</td>
								<td class="py-3">
									<div class="flex flex-wrap gap-1">
										{#if states[c.id]?.running}
											<button
												on:click={() => clientAction(c.id, 'stop')}
												class="px-2 py-1 bg-yellow-700 hover:bg-yellow-600 rounded text-xs"
											>stop</button>
											<button
												on:click={() => clientAction(c.id, 'restart')}
												class="px-2 py-1 bg-gray-700 hover:bg-gray-600 rounded text-xs"
											>restart</button>
										{:else}
											<button
												on:click={() => clientAction(c.id, 'start')}
												class="px-2 py-1 bg-green-700 hover:bg-green-600 rounded text-xs"
											>start</button>
										{/if}
										<button
											on:click={() => openLogs(c.id)}
											class="px-2 py-1 bg-gray-800 hover:bg-gray-700 rounded text-xs"
										>logs</button>
										<button
											on:click={() => (showQR = subUrl(c.id, c.sub_token))}
											class="px-2 py-1 bg-gray-800 hover:bg-gray-700 rounded text-xs"
										>QR</button>
										<button
											on:click={() => copyToClipboard(subUrl(c.id, c.sub_token))}
											class="px-2 py-1 bg-gray-800 hover:bg-gray-700 rounded text-xs"
										>copy sub</button>
										<button
											on:click={() => clientAction(c.id, 'rotate-key')}
											class="px-2 py-1 bg-gray-800 hover:bg-gray-700 rounded text-xs"
										>🔑</button>
										<button
											on:click={() => clientAction(c.id, 'delete')}
											class="px-2 py-1 bg-red-900 hover:bg-red-800 rounded text-xs"
										>del</button>
									</div>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</main>
</div>

<!-- QR modal -->
{#if showQR}
	<div
		class="fixed inset-0 bg-black/70 flex items-center justify-center z-50"
		on:click|self={() => (showQR = null)}
	>
		<div class="bg-gray-900 rounded-xl p-6 max-w-sm w-full mx-4 text-center">
			<h2 class="text-lg font-bold mb-4">Subscription QR</h2>
			<img src={subQrUrl(showQR.split('/')[4], showQR.split('/')[5])} alt="QR" class="mx-auto rounded" />
			<p class="mt-3 text-xs text-gray-500 break-all">{showQR}</p>
			<button
				on:click={() => copyToClipboard(showQR ?? '')}
				class="mt-3 px-4 py-2 bg-blue-600 hover:bg-blue-500 rounded-lg text-sm"
			>Copy link</button>
			<button
				on:click={() => (showQR = null)}
				class="mt-2 block mx-auto text-sm text-gray-500 hover:text-gray-300"
			>Close</button>
		</div>
	</div>
{/if}

<!-- Create client modal -->
{#if showCreate}
	<div
		class="fixed inset-0 bg-black/70 flex items-center justify-center z-50"
		on:click|self={() => (showCreate = false)}
	>
		<div class="bg-gray-900 rounded-xl p-6 max-w-lg w-full mx-4">
			<h2 class="text-lg font-bold mb-5">New client</h2>
			<form on:submit|preventDefault={createClient} class="space-y-4">
				<div class="grid grid-cols-2 gap-4">
					<div>
						<label class="text-xs text-gray-400 block mb-1">Name *</label>
						<input bind:value={newName} required
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
					<div>
						<label class="text-xs text-gray-400 block mb-1">Comment</label>
						<input bind:value={newComment}
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
				</div>
				<div class="grid grid-cols-2 gap-4">
					<div>
						<label class="text-xs text-gray-400 block mb-1">Subdomain *</label>
						<input bind:value={newSubdomain} required placeholder="ilte0310"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
					<div>
						<label class="text-xs text-gray-400 block mb-1">Room ID *</label>
						<input bind:value={newRoomID} required placeholder="abcdef123456"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
				</div>
				<div class="grid grid-cols-3 gap-4">
					<div>
						<label class="text-xs text-gray-400 block mb-1">Speed (Mbps)</label>
						<input type="number" bind:value={newSpeedMbps} min="0"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
					<div>
						<label class="text-xs text-gray-400 block mb-1">Traffic (GB)</label>
						<input type="number" bind:value={newTrafficGB} min="0"
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
					<div>
						<label class="text-xs text-gray-400 block mb-1">Expires at</label>
						<input type="date" bind:value={newExpiresAt}
							class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm focus:border-blue-500 outline-none" />
					</div>
				</div>
				<div class="flex gap-3 pt-2">
					<button
						type="submit"
						disabled={creating}
						class="flex-1 py-2 bg-blue-600 hover:bg-blue-500 disabled:opacity-50 rounded-lg text-sm font-medium transition-colors"
					>{creating ? 'Creating…' : 'Create'}</button>
					<button
						type="button"
						on:click={() => (showCreate = false)}
						class="px-4 py-2 bg-gray-800 hover:bg-gray-700 rounded-lg text-sm"
					>Cancel</button>
				</div>
			</form>
		</div>
	</div>
{/if}

<!-- Logs modal -->
{#if showLogs}
	<div
		class="fixed inset-0 bg-black/70 flex items-center justify-center z-50"
		on:click|self={closeLogs}
	>
		<div class="bg-gray-900 rounded-xl p-4 w-full max-w-3xl mx-4 max-h-[80vh] flex flex-col">
			<div class="flex items-center justify-between mb-3">
				<h2 class="font-bold">Logs — {showLogs}</h2>
				<button on:click={closeLogs} class="text-gray-500 hover:text-gray-300 text-xl">×</button>
			</div>
			<div class="flex-1 overflow-y-auto bg-gray-950 rounded-lg p-3 font-mono text-xs leading-5">
				{#each logsContent as line}
					<div class="text-gray-300">{line}</div>
				{:else}
					<div class="text-gray-600">No logs yet</div>
				{/each}
			</div>
		</div>
	</div>
{/if}
