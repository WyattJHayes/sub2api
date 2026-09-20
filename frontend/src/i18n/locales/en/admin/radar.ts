export default {
  radar: {
    title: 'Quality Radar',
    description: 'Model quality, reliability and release decisions in one workspace.',
    pages: {
      overview: 'Overview',
      models: 'Models',
      runs: 'Evaluation runs',
      alerts: 'Alerts',
      gates: 'Release gates',
      workers: 'Workers',
      datasets: 'Datasets'
    },
    actions: {
      refresh: 'Refresh',
      addModel: 'Add model',
      addingModel: 'Adding...',
      untrack: 'Untrack',
      untrackHint: 'Untrack this model while retaining quality history',
      untrackConfirm: 'Untrack this model? Its quality history will remain.'
    },
    table: {
      model: 'Model',
      domain: 'Domain',
      health: 'Health',
      delta: 'Delta',
      ci: 'CI',
      samples: 'Samples',
      p99: 'P99 latency',
      status: 'Status'
    },
    empty: {
      models: 'No model observations available'
    },
    status: {
      healthy: 'Healthy',
      degraded: 'Degraded',
      blocked: 'Blocked',
      insufficient_evidence: 'Insufficient evidence',
      passed: 'Passed',
      active: 'Active',
      resolved: 'Resolved',
      open: 'Open',
      stale: 'Stale',
      disabled: 'Disabled',
      acknowledged: 'Acknowledged',
      review_required: 'Review required',
      waived: 'Waived',
      recorded: 'Recorded',
      draft: 'Draft',
      published: 'Published',
      manual: 'Manual',
      bound: 'Bound',
      'legacy-unbound': 'Legacy unbound',
      pending: 'Pending',
      running: 'Running',
      budget_paused: 'Budget paused',
      paused: 'Paused',
      completed: 'Completed',
      cancelled: 'Cancelled',
      failed: 'Failed'
    },
    domains: {
      aggregate: 'Aggregate',
      coding: 'Coding',
      reasoning: 'Reasoning',
      instruction: 'Instruction following',
      long_context: 'Long context',
      tool_call: 'Tool calling',
      protocol: 'Protocol compatibility',
      safety: 'Safety',
      performance: 'Performance',
      cost: 'Cost'
    },
    overview: {
      modelHealth: 'Model health',
      openAlerts: 'Open alerts',
      noOpenAlerts: 'No open alerts',
      metrics: {
        models: 'Models',
        openAlerts: 'Open alerts',
        blockedGates: 'Blocked gates',
        healthyWorkers: 'Healthy workers'
      }
    },
    alerts: {
      title: 'Alert lifecycle',
      cause: 'Cause',
      severity: 'Severity',
      causes: {
        upstream_model: 'Upstream model',
        channel_or_pool: 'Channel or pool',
        gateway_protocol: 'Gateway protocol',
        service_quality: 'Service quality',
        insufficient_evidence: 'Insufficient evidence'
      }
    },
    gates: {
      title: 'Release gates',
      empty: 'No gate decisions recorded'
    },
    workers: {
      title: 'Worker health',
      heartbeat: 'Heartbeat: {time}',
      kinds: {
        runner: 'Runner worker',
        grader: 'Grader worker',
        statistics: 'Statistics worker'
      }
    },
    datasets: {
      title: 'Evaluation datasets',
      description: 'Versioned prompts and grading contracts used by Radar runs.',
      newDataset: 'New dataset',
      createDataset: 'Create dataset',
      creating: 'Creating...',
      publish: 'Publish',
      publishing: 'Publishing...',
      empty: 'No datasets available',
      table: {
        dataset: 'Dataset',
        version: 'Version',
        cases: 'Cases',
        source: 'Source',
        creator: 'Creator',
        tenant: 'Tenant ID'
      },
      sourceTypes: {
        synthetic: 'Synthetic',
        public: 'Public',
        imported: 'Imported'
      },
      graders: {
        exact: 'Exact match',
        llm_judge: 'LLM judge',
        json_schema: 'JSON schema'
      },
      createDialog: {
        title: 'New evaluation dataset',
        datasetKey: 'Dataset key',
        datasetKeyPlaceholder: 'For example reasoning-smoke',
        versionPlaceholder: 'For example 2026-07-27',
        source: 'Source',
        initialCase: 'Initial case',
        caseKey: 'Case key',
        caseKeyPlaceholder: 'For example addition-1',
        capability: 'Capability',
        priority: 'Priority',
        prompt: 'Prompt',
        promptPlaceholder: 'Enter the controlled prompt',
        expectedOutput: 'Expected output',
        expectedOutputPlaceholder: 'Enter the expected answer or rubric',
        weight: 'Weight',
        samples: 'Samples',
        grader: 'Grader',
        estimatedCost: 'Estimated cost',
        confidentiality: 'Confidentiality',
        gatewayPath: 'Gateway path'
      }
    },
    runs: {
      title: 'Evaluation runs',
      description: 'Paired baseline and candidate executions with frozen configuration.',
      newPlan: 'New plan',
      createPlan: 'Create plan',
      creatingPlan: 'Creating...',
      startRun: 'Start run',
      startingRun: 'Starting...',
      enableEvaluationKey: 'Enable for Radar',
      enablingEvaluationKey: 'Enabling...',
      empty: 'No evaluation runs recorded',
      budgetHint: 'Costs are in USD. Reserved cost is an estimate. Budgets are soft guards: new claims stop when recorded costs reach the limit, but in-flight requests and delayed billing may exceed it. A plan budget is not an account-wide cap.',
      costPending: 'Not yet recorded',
      costPartial: 'Some requests have not been billed',
      billingProgress: 'Billed evidence {billed}/{total}',
      pause: 'Pause',
      pausing: 'Pausing...',
      pauseReasons: {
        budget: 'Recorded cost limit reached',
        operator: 'Paused by administrator'
      },
      pauseDialog: {
        title: 'Pause this evaluation run?',
        description: 'Stop claiming new tasks. In-flight requests will finish and may still incur charges. Verify the run ID.'
      },
      table: {
        run: 'Run',
        plan: 'Plan',
        trigger: 'Trigger',
        budgetLimit: 'Budget limit',
        reservedCost: 'Reserved cost (estimate)',
        actualCost: 'Recorded cost',
        createdAt: 'Created'
      },
      triggers: {
        manual: 'Manual',
        cron: 'Scheduled',
        release: 'Release',
        event: 'Event',
        recovery: 'Recovery'
      },
      planDialog: {
        title: 'New evaluation plan',
        name: 'Plan name',
        namePlaceholder: 'For example DeepSeek regression',
        dataset: 'Published dataset',
        selectDataset: 'Select a dataset',
        gatewayAPIKey: 'Gateway API key ID',
        triggerType: 'Trigger',
        triggerManual: 'Manual',
        triggerCron: 'Scheduled',
        cronExpression: 'Schedule (minute hour day month weekday)',
        baselineRelease: 'Baseline release reference',
        candidateRelease: 'Candidate release reference',
        releasePlaceholder: 'e.g. v0.2.7-sol',
        modelMatrix: 'Paired model matrix',
        parametersHint: 'Request parameters come from the published case prompt_spec. Plans select routes without overriding temperature or output limits.',
        logicalRoute: 'Logical route',
        logicalRoutePlaceholder: 'For example deepseek-chat',
        baselineRoute: 'Baseline route',
        baselineRoutePlaceholder: 'For example deepseek-chat-v1',
        candidateRoute: 'Candidate route',
        candidateRoutePlaceholder: 'For example deepseek-chat-v2',
        runCostLimit: 'Run cost limit',
        dailyCostLimit: 'Daily cost limit',
        maxConcurrency: 'Max concurrency'
      },
      runDialog: {
        title: 'Start evaluation run',
        planId: 'Plan ID',
        planIdPlaceholder: 'Plan UUID',
        baselineRef: 'Baseline release reference',
        baselineRefPlaceholder: 'For example baseline-2026-07-20',
        candidateRef: 'Candidate release reference',
        candidateRefPlaceholder: 'For example candidate-2026-07-27'
      }
    },
    models: {
      title: 'Tracked models',
      description: 'New models remain evidence-free until Radar completes an evaluation aggregate.',
      alias: 'Model alias',
      aliasPlaceholder: 'For example gpt-5.6-sol'
    },
    messages: {
      loadFailed: 'Unable to load radar data',
      sectionLoadFailed: 'Unable to load radar section',
      modelAliasRequired: 'Enter a model alias',
      modelAdded: 'Model added to Radar tracking',
      modelAddFailed: 'Failed to add model',
      modelUntracked: 'Model tracking removed',
      modelUntrackFailed: 'Failed to remove model tracking',
      datasetFieldsRequired: 'Complete all required dataset fields',
      datasetCreated: 'Dataset created',
      datasetCreateFailed: 'Failed to create dataset',
      datasetPublished: 'Dataset published',
      datasetPublishFailed: 'Failed to publish dataset',
      evaluationKeyRequired: 'Enter a valid API key ID',
      evaluationKeyEnabled: 'API key enabled for Radar',
      evaluationKeyEnableFailed: 'Failed to enable API key',
      planFieldsRequired: 'Complete all required plan fields',
      planCreated: 'Evaluation plan created',
      planCreateFailed: 'Failed to create evaluation plan',
      runReferencesRequired: 'Complete all run references',
      runStarted: 'Evaluation run started',
      runStartFailed: 'Failed to start evaluation run',
      runPaused: 'Evaluation run paused',
      runPauseFailed: 'Failed to pause evaluation run; retry this confirmation',
      runPauseUnconfirmed: 'Pause not confirmed; retry or refresh to verify the run status'
    }
  }
}
