//go:build noexternal

package cli

// Local-only build: no credentials / dataegress / providerbudget. Web search is
// refused at runner build ("web search requires External mode"); the shared
// analystWebQueryAllows stays false; loopBudgetAdapt is turn-ceiling only.
