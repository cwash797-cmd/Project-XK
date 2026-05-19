<script lang="ts">
	import { api } from '$lib/api';

	let password = '';
	let confirm = '';
	let error = '';
	let loading = false;

	async function setup() {
		error = '';
		if (password !== confirm) { error = 'Passwords do not match'; return; }
		if (password.length < 8) { error = 'Password must be at least 8 characters'; return; }
		loading = true;
		try {
			await api.setup(password);
			window.location.href = '/';
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}
</script>

<svelte:head><title>Setup — Admin Panel</title></svelte:head>

<div class="min-h-screen flex items-center justify-center bg-gray-950">
	<div class="w-full max-w-sm bg-gray-900 rounded-2xl p-8 shadow-xl">
		<h1 class="text-2xl font-bold text-white mb-2 text-center">⚡ Admin Panel</h1>
		<p class="text-sm text-gray-500 text-center mb-6">First run — set your admin password</p>
		{#if error}
			<div class="mb-4 p-3 bg-red-900/40 border border-red-700 rounded-lg text-sm text-red-300">{error}</div>
		{/if}
		<form on:submit|preventDefault={setup} class="space-y-4">
			<div>
				<label for="pwd" class="text-xs text-gray-400 block mb-1">Password</label>
				<input id="pwd" type="password" bind:value={password} required
					class="w-full bg-gray-800 border border-gray-700 rounded-lg px-4 py-3 text-sm focus:border-blue-500 outline-none" />
			</div>
			<div>
				<label for="pwd-confirm" class="text-xs text-gray-400 block mb-1">Confirm password</label>
				<input id="pwd-confirm" type="password" bind:value={confirm} required
					class="w-full bg-gray-800 border border-gray-700 rounded-lg px-4 py-3 text-sm focus:border-blue-500 outline-none" />
			</div>
			<button type="submit" disabled={loading}
				class="w-full py-3 bg-blue-600 hover:bg-blue-500 disabled:opacity-50 rounded-lg font-medium transition-colors"
			>{loading ? 'Saving…' : 'Set password'}</button>
		</form>
	</div>
</div>
