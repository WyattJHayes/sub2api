export default {
  radar: {
    title: '质量雷达',
    description: '在统一工作区查看模型质量、可靠性与发布决策。',
    pages: {
      overview: '概览',
      models: '模型',
      runs: '评测运行',
      alerts: '告警',
      gates: '发布门禁',
      workers: '执行器',
      datasets: '数据集'
    },
    actions: {
      refresh: '刷新',
      addModel: '添加模型',
      addingModel: '添加中...',
      untrack: '解除跟踪',
      untrackHint: '解除跟踪，历史质量报告仍会保留',
      untrackConfirm: '解除跟踪该模型？历史质量报告会保留。'
    },
    table: {
      model: '模型',
      domain: '能力域',
      health: '健康状态',
      delta: '变化',
      ci: '置信区间',
      samples: '样本数',
      p99: 'P99 延迟',
      status: '状态'
    },
    empty: {
      models: '暂无模型观测数据'
    },
    status: {
      healthy: '健康',
      degraded: '降级',
      blocked: '已阻止',
      insufficient_evidence: '证据不足',
      passed: '已通过',
      active: '活跃',
      resolved: '已解决',
      open: '待处理',
      stale: '已过期',
      disabled: '已停用',
      acknowledged: '已确认',
      review_required: '需要复核',
      waived: '已豁免',
      recorded: '已记录',
      draft: '草稿',
      published: '已发布',
      manual: '手动',
      bound: '已绑定',
      'legacy-unbound': '历史未绑定',
      pending: '等待中',
      running: '运行中',
      budget_paused: '预算暂停',
      paused: '已暂停',
      completed: '已完成',
      cancelled: '已取消',
      failed: '失败'
    },
    domains: {
      aggregate: '综合',
      coding: '编程',
      reasoning: '推理',
      instruction: '指令遵循',
      long_context: '长上下文',
      tool_call: '工具调用',
      protocol: '协议兼容',
      safety: '安全',
      performance: '性能',
      cost: '成本'
    },
    overview: {
      modelHealth: '模型健康',
      openAlerts: '待处理告警',
      noOpenAlerts: '暂无待处理告警',
      metrics: {
        models: '模型',
        openAlerts: '待处理告警',
        blockedGates: '已阻止门禁',
        healthyWorkers: '健康执行器'
      }
    },
    alerts: {
      title: '告警生命周期',
      cause: '原因',
      severity: '严重程度',
      causes: {
        upstream_model: '上游模型',
        channel_or_pool: '通道或资源池',
        gateway_protocol: '网关协议',
        service_quality: '服务质量',
        insufficient_evidence: '证据不足'
      }
    },
    gates: {
      title: '发布门禁',
      empty: '暂无门禁决策记录'
    },
    workers: {
      title: '执行器健康',
      heartbeat: '心跳：{time}',
      kinds: {
        runner: '运行执行器',
        grader: '评分执行器',
        statistics: '统计执行器'
      }
    },
    datasets: {
      title: '评测数据集',
      description: '供雷达运行使用的版本化提示词与评分约定。',
      newDataset: '新建数据集',
      createDataset: '创建数据集',
      creating: '创建中...',
      publish: '发布',
      publishing: '发布中...',
      empty: '暂无数据集',
      table: {
        dataset: '数据集',
        version: '版本',
        cases: '用例数',
        source: '来源',
        creator: '创建者',
        tenant: '租户 ID'
      },
      sourceTypes: {
        synthetic: '合成',
        public: '公开',
        imported: '已导入'
      },
      graders: {
        exact: '精确匹配',
        llm_judge: '大模型裁判',
        json_schema: 'JSON Schema'
      },
      createDialog: {
        title: '新建评测数据集',
        datasetKey: '数据集标识',
        datasetKeyPlaceholder: '例如 reasoning-smoke',
        versionPlaceholder: '例如 2026-07-27',
        source: '来源',
        initialCase: '初始用例',
        caseKey: '用例标识',
        caseKeyPlaceholder: '例如 addition-1',
        capability: '能力域',
        priority: '优先级',
        prompt: '提示词',
        promptPlaceholder: '输入受控提示词',
        expectedOutput: '预期输出',
        expectedOutputPlaceholder: '输入预期答案或评分标准',
        weight: '权重',
        samples: '采样次数',
        grader: '评分器',
        estimatedCost: '预估成本',
        confidentiality: '保密级别',
        gatewayPath: '网关路径'
      }
    },
    runs: {
      title: '评测运行',
      description: '基线与候选模型在冻结配置下成对执行。',
      newPlan: '新建计划',
      createPlan: '创建计划',
      creatingPlan: '创建中...',
      startRun: '开始运行',
      startingRun: '启动中...',
      enableEvaluationKey: '启用雷达评测',
      enablingEvaluationKey: '启用中...',
      empty: '暂无评测运行记录',
      table: {
        run: '运行',
        plan: '计划',
        trigger: '触发方式',
        reservedCost: '保留成本',
        createdAt: '创建时间'
      },
      triggers: {
        manual: '手动',
        cron: '定时',
        release: '发布',
        event: '事件',
        recovery: '恢复'
      },
      planDialog: {
        title: '新建评测计划',
        name: '计划名称',
        namePlaceholder: '例如 DeepSeek 回归',
        dataset: '已发布数据集',
        selectDataset: '选择数据集',
        gatewayAPIKey: '网关 API 密钥 ID',
        modelMatrix: '成对模型矩阵',
        logicalRoute: '逻辑路由',
        logicalRoutePlaceholder: '例如 deepseek-chat',
        baselineRoute: '基线路由',
        baselineRoutePlaceholder: '例如 deepseek-chat-v1',
        candidateRoute: '候选路由',
        candidateRoutePlaceholder: '例如 deepseek-chat-v2',
        runCostLimit: '单次运行成本上限',
        dailyCostLimit: '每日成本上限',
        maxConcurrency: '最大并发数'
      },
      runDialog: {
        title: '开始评测运行',
        planId: '计划 ID',
        planIdPlaceholder: '计划 UUID',
        baselineRef: '基线发布引用',
        baselineRefPlaceholder: '例如 baseline-2026-07-20',
        candidateRef: '候选发布引用',
        candidateRefPlaceholder: '例如 candidate-2026-07-27'
      }
    },
    models: {
      title: '跟踪模型',
      description: '新加入的模型会先显示证据不足，评测产生聚合结果后再显示质量指标。',
      alias: '模型别名',
      aliasPlaceholder: '例如 gpt-5.6-sol'
    },
    messages: {
      loadFailed: '无法加载雷达数据',
      sectionLoadFailed: '无法加载雷达页面数据',
      modelAliasRequired: '请输入模型别名',
      modelAdded: '模型已加入雷达跟踪',
      modelAddFailed: '添加模型失败',
      modelUntracked: '模型已解除跟踪',
      modelUntrackFailed: '解除模型跟踪失败',
      datasetFieldsRequired: '请填写所有必填数据集字段',
      datasetCreated: '数据集已创建',
      datasetCreateFailed: '创建数据集失败',
      datasetPublished: '数据集已发布',
      datasetPublishFailed: '发布数据集失败',
      evaluationKeyRequired: '请输入有效的 API 密钥 ID',
      evaluationKeyEnabled: 'API 密钥已启用雷达评测',
      evaluationKeyEnableFailed: '启用 API 密钥失败',
      planFieldsRequired: '请填写所有必填计划字段',
      planCreated: '评测计划已创建',
      planCreateFailed: '创建评测计划失败',
      runReferencesRequired: '请填写所有运行引用',
      runStarted: '评测运行已启动',
      runStartFailed: '启动评测运行失败'
    }
  }
}
