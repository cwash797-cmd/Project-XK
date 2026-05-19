<script lang="ts">
	import { api } from '$lib/api';

	let password = '';
	let error = '';
	let loading = false;

	async function login() {
		error = '';
		loading = true;
		try {
			await api.login(password);
			window.location.href = '/';
		} catch (e: any) {
			error = e.message;
		} finally {
			loading = false;
		}
	}
</script>

<svelte:head><title>Login — ktalk panel</title></svelte:head>

<div class="min-h-screen flex items-center justify-center bg-gray-950">
	<div class="w-full max-w-sm bg-gray-900 rounded-2xl p-8 shadow-xl">
		<h1 class="text-2xl font-bold text-white mb-6 text-center">⚡ ktalk panel</h1>
		{#if error}
			<div class="mb-4 p-3 bg-red-900/40 border border-red-700 rounded-lg text-sm text-red-300">{error}</div>
		{/if}
		<form on:submit|preventDefault={login} class="space-y-4">
			<div>
				<label class="text-xs text-gray-400 block mb-1">Admin password</label>
				<input
					type="password"
					bind:value={password}
					required
					autofocus
					class="w-full bg-gray-800 border border-gray-700 rounded-lg px-4 py-3 text-sm focus:border-blue-500 outline-none"
				/>
			</div>
			<button
				type="submit"
				disabled={loading}
				class="w-full py-3 bg-blue-600 hover:bg-blue-500 disabled:opacity-50 rounded-lg font-medium transition-colors"
			>{loading ? 'Logging in…' : 'Login'}</button>
		</form>
	</div>
</div>
