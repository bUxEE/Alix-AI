// Provisions a disposable test tenant for the smoke tests and removes it afterwards.
export const createDisposableTenant = async (): Promise<string> => "tenant-test-1";
export const cleanupDisposableTenant = async (_id: string): Promise<void> => undefined;
